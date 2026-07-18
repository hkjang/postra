// Command postra is the single-binary MVP: REST API server, MCP server
// (stdio + Streamable HTTP), sync worker, and CLI in one executable.
// Internal packages keep the boundaries of §15 so the processes can be
// split later (mail-api / mail-mcp / mail-worker).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"postra/internal/adapters/ai"
	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/adapters/pgstore"
	"postra/internal/adapters/pop3"
	"postra/internal/adapters/secretstore"
	adsmtp "postra/internal/adapters/smtp"
	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
	"postra/internal/transport/httpapi"
	"postra/internal/transport/mcpserver"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if err := run(cmd, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `postra — personal mail AI & MCP platform

Usage:
  postra init                        write a default config file
  postra serve                       run REST API + remote MCP (Streamable HTTP)
  postra mcp                         run MCP server on stdio (for local MCP clients)

  postra secret set    --type mail_password|api_key --label <label> [--value-stdin]
  postra secret rotate --ref <secret_ref> [--value-stdin]
  postra secret revoke --ref <secret_ref>

  postra account add   --name .. --email .. --pop3-host .. [--pop3-security tls|starttls|none] ...
  postra account list
  postra account test  --id <account_id>

  postra sync          --account <account_id> [--max N] [--wait]
  postra search        --q <text> [--limit N]
  postra job           --id <job_id>

Global flags: --config <path> (default ~/.postra/config.json), or POSTRA_* env vars.
`)
}

func loadApp(configPath string) (*application.App, config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, cfg, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, cfg, err
	}
	kek, err := crypto.LoadOrCreateKEK(cfg.DataDir)
	if err != nil {
		return nil, cfg, err
	}

	store, err := openStorage(cfg, kek)
	if err != nil {
		return nil, cfg, err
	}

	local, err := objectstore.NewLocal(cfg.DataDir)
	if err != nil {
		return nil, cfg, err
	}
	var objects objectstore.Store = local
	if cfg.EncryptAtRest {
		objects = objectstore.NewEncrypted(local, kek)
	}
	secrets := secretstore.NewLocal(cfg.DataDir, kek)
	app, err := application.New(cfg, store, objects, secrets,
		pop3.Dialer{}, adsmtp.Client{}, ai.New(cfg.AI, secrets))
	return app, cfg, err
}

// openStorage selects the persistence backend. SQLite is the personal/
// embedded default (with optional at-rest body encryption); PostgreSQL is
// the server/multi-user backend with pgvector semantic search.
func openStorage(cfg config.Config, kek *crypto.KEK) (application.Storage, error) {
	switch cfg.StorageDriver {
	case "", "sqlite":
		st, err := persistence.Open(filepath.Join(cfg.DataDir, "postra.db"))
		if err != nil {
			return nil, err
		}
		if cfg.EncryptAtRest {
			st.EnableEncryption(kek) // body-column encryption (SQLite only)
		}
		return st, nil
	case "postgres":
		if cfg.PostgresDSN == "" {
			return nil, fmt.Errorf("storage_driver=postgres requires postgres_dsn")
		}
		return pgstore.Open(context.Background(), cfg.PostgresDSN)
	default:
		return nil, fmt.Errorf("unknown storage_driver %q (sqlite|postgres)", cfg.StorageDriver)
	}
}

func run(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")

	switch cmd {
	case "init":
		fs.Parse(args)
		cfg := config.Default()
		path := *configPath
		if path == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".postra", "config.json")
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists at %s", path)
		}
		if err := cfg.Save(path); err != nil {
			return err
		}
		fmt.Println("wrote", path)
		return nil

	case "serve":
		fs.Parse(args)
		return serve(*configPath)

	case "mcp":
		fs.Parse(args)
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		defer app.Shutdown()
		return mcpserver.RunStdio(context.Background(), app)

	case "key":
		return keyCmd(args)

	case "secret":
		return secretCmd(args)

	case "account":
		return accountCmd(args)

	case "sync":
		maxN := fs.Int("max", 0, "max messages this run")
		account := fs.String("account", "", "account ID")
		wait := fs.Bool("wait", false, "wait for completion")
		fs.Parse(args)
		if *account == "" {
			return errors.New("--account is required")
		}
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		ctx := application.WithActor(context.Background(), "cli")
		job, err := app.StartSync(ctx, *account, application.SyncOptions{MaxMessages: *maxN})
		if err != nil {
			return err
		}
		fmt.Println("job:", job.ID)
		if *wait {
			for {
				time.Sleep(time.Second)
				j, err := app.GetJob(ctx, job.ID)
				if err != nil {
					return err
				}
				if j.Status != domain.JobQueued && j.Status != domain.JobRunning {
					printJSON(j)
					break
				}
			}
		}
		app.Shutdown()
		return nil

	case "search":
		q := fs.String("q", "", "search text")
		limit := fs.Int("limit", 20, "max results")
		fs.Parse(args)
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		defer app.Shutdown()
		res, err := app.Search(application.WithActor(context.Background(), "cli"),
			domain.SearchQuery{Text: *q, Limit: *limit})
		if err != nil {
			return err
		}
		printJSON(res)
		return nil

	case "job":
		id := fs.String("id", "", "job ID")
		fs.Parse(args)
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		defer app.Shutdown()
		j, err := app.GetJob(application.WithActor(context.Background(), "cli"), *id)
		if err != nil {
			return err
		}
		printJSON(j)
		return nil

	case "help", "-h", "--help":
		usage()
		return nil
	}
	usage()
	return fmt.Errorf("unknown command %q", cmd)
}

func serve(configPath string) error {
	app, cfg, err := loadApp(configPath)
	if err != nil {
		return err
	}
	defer app.Shutdown()

	warnIfNonLoopback(cfg)

	// Background scheduler: recover interrupted jobs, then periodic sync.
	schedCtx, schedCancel := context.WithCancel(context.Background())
	defer schedCancel()
	go app.RunScheduler(schedCtx)
	go app.RunRetryWorker(schedCtx)

	restSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(app, cfg.APIToken).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 2)
	go func() {
		slog.Info("REST API listening", "addr", cfg.HTTPAddr)
		errCh <- restSrv.ListenAndServe()
	}()

	var mcpSrv *http.Server
	if cfg.MCPHTTPAddr != "" {
		mcpSrv = &http.Server{
			Addr:              cfg.MCPHTTPAddr,
			Handler:           mcpserver.HTTPHandler(app, cfg.APIToken),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.Info("MCP (Streamable HTTP) listening", "addr", cfg.MCPHTTPAddr)
			errCh <- mcpSrv.ListenAndServe()
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-stop:
		slog.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		restSrv.Shutdown(ctx)
		if mcpSrv != nil {
			mcpSrv.Shutdown(ctx)
		}
		return nil
	}
}

// warnIfNonLoopback flags a remote-exposed deployment without a token —
// legitimate on isolated networks, dangerous anywhere else (§10.1).
func warnIfNonLoopback(cfg config.Config) {
	for _, addr := range []string{cfg.HTTPAddr, cfg.MCPHTTPAddr} {
		if addr == "" {
			continue
		}
		host := addr[:strings.LastIndex(addr, ":")]
		if host != "127.0.0.1" && host != "localhost" && host != "::1" && cfg.APIToken == "" {
			slog.Warn("endpoint bound to a non-loopback interface WITHOUT api_token — acceptable only on isolated networks",
				"addr", addr)
		}
	}
}

// ---------- key subcommands ----------

// keyCmd handles KEK lifecycle: rotate generates a new key version and
// rewraps existing secrets, encrypted objects, and encrypted body columns
// under it (§11.3 회전, SEC-KEY-010/012).
func keyCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: postra key rotate [--retire-old]")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("key "+sub, flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")
	retireOld := fs.Bool("retire-old", false, "retire all prior key versions after rewrap")
	fs.Parse(rest)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	kek, err := crypto.LoadOrCreateKEK(cfg.DataDir)
	if err != nil {
		return err
	}

	switch sub {
	case "rotate":
		if cfg.StorageDriver == "postgres" {
			return errors.New("key rotate currently supports the sqlite backend (body-column encryption)")
		}
		prev := kek.CurrentVersion()
		nv, err := kek.Rotate()
		if err != nil {
			return err
		}
		fmt.Printf("rotated KEK: v%d -> v%d\n", prev, nv)

		secrets := secretstore.NewLocal(cfg.DataDir, kek)
		ns, err := secrets.RewrapAll(context.Background())
		if err != nil {
			return fmt.Errorf("rewrap secrets: %w", err)
		}
		fmt.Printf("rewrapped secrets: %d\n", ns)

		if cfg.EncryptAtRest {
			local, err := objectstore.NewLocal(cfg.DataDir)
			if err != nil {
				return err
			}
			no, err := objectstore.NewEncrypted(local, kek).RewrapAll()
			if err != nil {
				return fmt.Errorf("rewrap objects: %w", err)
			}
			fmt.Printf("rewrapped objects: %d\n", no)

			store, err := persistence.Open(filepath.Join(cfg.DataDir, "postra.db"))
			if err != nil {
				return err
			}
			defer store.Close()
			store.EnableEncryption(kek)
			nb, err := store.RewrapBodies(context.Background())
			if err != nil {
				return fmt.Errorf("rewrap bodies: %w", err)
			}
			fmt.Printf("rewrapped bodies: %d\n", nb)
		}

		if *retireOld {
			for v := 1; v < nv; v++ {
				kek.RetireVersion(v)
			}
			fmt.Printf("retired key versions 1..%d\n", nv-1)
		}
		fmt.Println("key rotation complete")
		return nil
	}
	return fmt.Errorf("unknown key subcommand %q", sub)
}

// ---------- secret subcommands ----------

func secretCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: postra secret set|rotate|revoke ...")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("secret "+sub, flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")
	typ := fs.String("type", "mail_password", "mail_password | api_key")
	label := fs.String("label", "", "human-readable label")
	ref := fs.String("ref", "", "secret reference")
	valueStdin := fs.Bool("value-stdin", false, "read the secret from stdin instead of the TTY prompt")
	fs.Parse(rest)

	app, _, err := loadApp(*configPath)
	if err != nil {
		return err
	}
	defer app.Shutdown()
	ctx := application.WithActor(context.Background(), "cli")

	readSecret := func() (*domain.SecretHandle, error) {
		if *valueStdin {
			var v string
			if _, err := fmt.Fscanln(os.Stdin, &v); err != nil {
				return nil, err
			}
			h := domain.NewSecretHandle([]byte(v))
			v = ""
			return h, nil
		}
		// SEC-KEY-002: no-echo TTY input keeps the value out of shell
		// history, process args, and logs.
		fmt.Fprint(os.Stderr, "Secret value (input hidden): ")
		b, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, err
		}
		h := domain.NewSecretHandle(b)
		for i := range b {
			b[i] = 0
		}
		return h, nil
	}

	switch sub {
	case "set":
		h, err := readSecret()
		if err != nil {
			return err
		}
		r, err := app.RegisterSecret(ctx, domain.SecretType(*typ), *label, h)
		if err != nil {
			return err
		}
		fmt.Println("secret_ref:", r)
		return nil
	case "rotate":
		if *ref == "" {
			return errors.New("--ref is required")
		}
		h, err := readSecret()
		if err != nil {
			return err
		}
		if err := app.RotateSecret(ctx, domain.SecretRef(*ref), h); err != nil {
			return err
		}
		fmt.Println("rotated:", *ref)
		return nil
	case "revoke":
		if *ref == "" {
			return errors.New("--ref is required")
		}
		if err := app.RevokeSecret(ctx, domain.SecretRef(*ref)); err != nil {
			return err
		}
		fmt.Println("revoked:", *ref)
		return nil
	}
	return fmt.Errorf("unknown secret subcommand %q", sub)
}

// ---------- account subcommands ----------

func accountCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: postra account add|list|test|disable ...")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("account "+sub, flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")

	name := fs.String("name", "", "account display name")
	email := fs.String("email", "", "mail address")
	pop3Host := fs.String("pop3-host", "", "POP3 host")
	pop3Port := fs.Int("pop3-port", 0, "POP3 port (default by security mode)")
	pop3Sec := fs.String("pop3-security", "tls", "tls | starttls | none")
	pop3User := fs.String("pop3-user", "", "POP3 username")
	pop3Ref := fs.String("pop3-secret-ref", "", "secret reference for POP3 password")
	smtpHost := fs.String("smtp-host", "", "SMTP host")
	smtpPort := fs.Int("smtp-port", 0, "SMTP port")
	smtpSec := fs.String("smtp-security", "tls", "tls | starttls | none")
	smtpUser := fs.String("smtp-user", "", "SMTP username")
	smtpAuth := fs.String("smtp-auth", "auto", "auto | none")
	smtpRef := fs.String("smtp-secret-ref", "", "secret reference for SMTP password")
	id := fs.String("id", "", "account ID")
	fs.Parse(rest)

	app, _, err := loadApp(*configPath)
	if err != nil {
		return err
	}
	defer app.Shutdown()
	ctx := application.WithActor(context.Background(), "cli")

	switch sub {
	case "add":
		acc, err := app.CreateAccount(ctx, application.CreateAccountInput{
			Name: *name, Email: *email,
			POP3Host: *pop3Host, POP3Port: *pop3Port, POP3Security: *pop3Sec,
			POP3Username: *pop3User, POP3SecretRef: *pop3Ref,
			SMTPHost: *smtpHost, SMTPPort: *smtpPort, SMTPSecurity: *smtpSec,
			SMTPUsername: *smtpUser, SMTPAuth: *smtpAuth, SMTPSecretRef: *smtpRef,
		})
		if err != nil {
			return err
		}
		printJSON(acc)
		return nil
	case "list":
		accs, err := app.ListAccounts(ctx)
		if err != nil {
			return err
		}
		printJSON(accs)
		return nil
	case "test":
		if *id == "" {
			return errors.New("--id is required")
		}
		diags, err := app.TestAccount(ctx, *id)
		if err != nil {
			return err
		}
		printJSON(diags)
		return nil
	case "disable":
		if *id == "" {
			return errors.New("--id is required")
		}
		if err := app.DisableAccount(ctx, *id); err != nil {
			return err
		}
		fmt.Println("disabled:", *id)
		return nil
	}
	return fmt.Errorf("unknown account subcommand %q", sub)
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
