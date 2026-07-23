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
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"postra/internal/adapters/ai"
	"postra/internal/adapters/imap"
	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/adapters/pgstore"
	"postra/internal/adapters/pop3"
	"postra/internal/adapters/secretstore"
	adsmtp "postra/internal/adapters/smtp"
	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/build"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
	"postra/internal/platform/telemetry"
	"postra/internal/transport/httpapi"
	"postra/internal/transport/mcpserver"
	"postra/internal/transport/webui"
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
  postra version                     print version and build info
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
  postra search        --q <text> [--limit N] [--account ID] [--folder inbox|important|archive|snoozed] [--label L] [--hybrid] [--group-thread]
  postra thread        --id <thread_id>                 timeline (ordered, with bodies)
  postra batch         --action <a> --ids id1,id2 [--snooze-until N] [--label L]
  postra auth          --id <message_id>                SPF/DKIM/DMARC + sender risk
  postra inbox         [--account ID] [--limit N]       task-oriented triage view
  postra rules         list | apply --message <id> | delete --id <rule> | add --file <json>
  postra cards         extract --message <id> | list [--status] | status --id --status | export --id --target
  postra team          inbox [--status --assignee] | get --message <id> | assign --message --assignee | status --message --status | note --message --body
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
	if app != nil {
		app.IMAP = imap.Dialer{} // inbound adapter selected per-account by protocol
	}
	return app, cfg, err
}

func maskDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			u.User = url.UserPassword(u.User.Username(), "*****")
			return u.String()
		}
	}
	return dsn
}

// openStorage selects the persistence backend. SQLite is the personal/
// embedded default (with optional at-rest body encryption); PostgreSQL is
// the server/multi-user backend with pgvector semantic search.
func openStorage(cfg config.Config, kek *crypto.KEK) (application.Storage, error) {
	driver := cfg.StorageDriver
	postgresConfigured := strings.TrimSpace(cfg.PostgresDSN) != ""
	if postgresConfigured && (driver == "" || driver == "sqlite") && os.Getenv("POSTRA_STORAGE_DRIVER") != "sqlite" {
		driver = "postgres"
	}
	slog.Info("initializing storage backend",
		"driver", driver,
		"postgres_dsn_configured", postgresConfigured,
		"data_dir", cfg.DataDir,
	)

	switch driver {
	case "", "sqlite":
		st, err := persistence.Open(filepath.Join(cfg.DataDir, "postra.db"))
		if err != nil {
			slog.Error("failed to open sqlite database", "error", err, "path", filepath.Join(cfg.DataDir, "postra.db"))
			return nil, fmt.Errorf("sqlite storage open failed: %w", err)
		}
		if cfg.EncryptAtRest {
			st.EnableEncryption(kek) // body-column encryption (SQLite only)
		}
		slog.Info("successfully opened sqlite storage", "postgres_dsn_configured", postgresConfigured)
		return st, nil

	case "postgres":
		if !postgresConfigured {
			err := fmt.Errorf("storage_driver=postgres requires postgres_dsn")
			slog.Error("postgres storage initialization failed",
				"reason", "postgres_dsn is not configured or empty",
				"postgres_dsn_configured", false,
				"error", err,
			)
			return nil, err
		}
		slog.Info("connecting to postgres storage",
			"postgres_dsn_configured", true,
			"dsn_masked", maskDSN(cfg.PostgresDSN),
		)
		st, err := pgstore.Open(context.Background(), cfg.PostgresDSN)
		if err != nil {
			slog.Error("failed to connect to postgres database",
				"error", err,
				"reason", err.Error(),
				"postgres_dsn_configured", true,
				"dsn_masked", maskDSN(cfg.PostgresDSN),
			)
			return nil, fmt.Errorf("postgres storage open failed: %w", err)
		}
		if cfg.EncryptAtRest {
			st.EnableEncryption(kek) // body-column encryption (parity with sqlite)
		}
		slog.Info("successfully connected and migrated postgres storage", "postgres_dsn_configured", true)
		return st, nil

	default:
		err := fmt.Errorf("unknown storage_driver %q (sqlite|postgres)", driver)
		slog.Error("storage initialization failed", "error", err, "driver", driver)
		return nil, err
	}
}

func run(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")

	switch cmd {
	case "version":
		fs.Parse(args)
		fmt.Printf("postra %s (%s %s/%s)\n", build.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil

	case "init":
		fs.Parse(args)
		cfg := config.Default()
		path := *configPath
		if path == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".postra", "config.json")
		}
		if _, err := os.Stat(path); err == nil { // #nosec G703 -- config path from operator flag/home dir, not untrusted input
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
		maxN := fs.Int("max", 0, "max messages this run (0 = full sync)")
		fullSync := fs.Bool("full", false, "force full sync of all messages")
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
		opts := application.SyncOptions{MaxMessages: *maxN, FullSync: *fullSync || *maxN <= 0}
		job, err := app.StartSync(ctx, *account, opts)

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
		account := fs.String("account", "", "restrict to an account ID")
		folder := fs.String("folder", "", "folder view: inbox|important|archive|snoozed")
		label := fs.String("label", "", "restrict to a label")
		hybrid := fs.Bool("hybrid", false, "use hybrid (FTS + semantic RRF) search")
		groupThread := fs.Bool("group-thread", false, "aggregate hybrid results to one per thread")
		rerank := fs.Bool("rerank", false, "apply LLM cross-encoder reranking (hybrid only)")
		fs.Parse(args)
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		defer app.Shutdown()
		ctx := application.WithActor(context.Background(), "cli")
		if *hybrid {
			views, err := app.HybridSearch(ctx, application.HybridSearchOptions{
				Query: *q, AccountID: *account, Limit: *limit, GroupByThread: *groupThread, Rerank: *rerank,
			})
			if err != nil {
				return err
			}
			printJSON(map[string]any{"results": views, "count": len(views)})
			return nil
		}
		res, err := app.Search(ctx,
			domain.SearchQuery{Text: *q, Limit: *limit, AccountID: *account, Folder: *folder, Label: *label})
		if err != nil {
			return err
		}
		printJSON(res)
		return nil

	case "thread":
		id := fs.String("id", "", "thread ID")
		fs.Parse(args)
		if *id == "" {
			return errors.New("--id is required")
		}
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		defer app.Shutdown()
		tl, err := app.GetThreadTimeline(application.WithActor(context.Background(), "cli"), *id)
		if err != nil {
			return err
		}
		printJSON(map[string]any{"timeline": tl, "count": len(tl)})
		return nil

	case "auth":
		id := fs.String("id", "", "message ID")
		fs.Parse(args)
		if *id == "" {
			return errors.New("--id is required")
		}
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		defer app.Shutdown()
		res, err := app.InspectAuthentication(application.WithActor(context.Background(), "cli"), *id)
		if err != nil {
			return err
		}
		printJSON(res)
		return nil

	case "batch":
		action := fs.String("action", "", "archive|unarchive|mark_important|unmark_important|snooze|unsnooze|add_label|remove_label|delete")
		ids := fs.String("ids", "", "comma-separated message IDs")
		snoozeUntil := fs.Int64("snooze-until", 0, "unix seconds (for snooze)")
		label := fs.String("label", "", "label (for add_label/remove_label)")
		fs.Parse(args)
		if *action == "" || *ids == "" {
			return errors.New("--action and --ids are required")
		}
		var msgIDs []string
		for _, s := range strings.Split(*ids, ",") {
			if s = strings.TrimSpace(s); s != "" {
				msgIDs = append(msgIDs, s)
			}
		}
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		defer app.Shutdown()
		res, err := app.BatchUpdateMessages(application.WithActor(context.Background(), "cli"),
			application.BatchUpdateOptions{
				MessageIDs: msgIDs, Action: application.BatchAction(*action),
				SnoozedUntil: *snoozeUntil, Label: *label,
			})
		if err != nil {
			return err
		}
		printJSON(res)
		return nil

	case "inbox":
		account := fs.String("account", "", "account ID scope")
		limit := fs.Int("limit", 100, "inbox window size")
		fs.Parse(args)
		app, _, err := loadApp(*configPath)
		if err != nil {
			return err
		}
		defer app.Shutdown()
		inbox, err := app.WorkInbox(application.WithActor(context.Background(), "cli"), *account, *limit)
		if err != nil {
			return err
		}
		printJSON(inbox)
		return nil

	case "rules":
		return rulesCmd(args)

	case "cards":
		return cardsCmd(args)

	case "team":
		return teamCmd(args)

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

	// OpenTelemetry tracing (OTLP/HTTP). No-op unless enabled + endpoint set.
	if cfg.TelemetryEnabled {
		shutdown, terr := telemetry.Init(context.Background(), "postra", build.Version)
		if terr != nil {
			slog.Warn("telemetry init failed; continuing without tracing", "err", terr)
		} else {
			slog.Info("OpenTelemetry tracing enabled (OTLP/HTTP)")
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = shutdown(ctx)
			}()
		}
	}

	// Only the serve worker process contends for leadership; the elected leader
	// recovers interrupted jobs and runs the schedulers.
	app.StartLeaderElection()

	// Background scheduler: periodic sync, outbox retries, and real-time IMAP IDLE.
	schedCtx, schedCancel := context.WithCancel(context.Background())
	defer schedCancel()
	go app.RunScheduler(schedCtx)
	go app.RunRetryWorker(schedCtx)
	go app.RunIdleWorker(schedCtx)

	// REST, Web UI, and Streamable HTTP MCP share the primary listener. A
	// separate MCPHTTPAddr remains optional for existing deployments.
	root := http.NewServeMux()
	root.Handle("/", httpapi.New(app, cfg.APIToken).Handler())
	root.Handle("/mcp", mcpserver.HTTPHandler(app, cfg.APIToken))
	if cfg.WebUIEnabled {
		uiHandler := webui.New(app, cfg.APIToken).Handler()
		root.Handle("/ui/", uiHandler)
		root.Handle("/favicon.ico", uiHandler)
		root.Handle("/favicon.png", uiHandler)
		root.Handle("/logo.png", uiHandler)
		slog.Info("web UI enabled", "path", "/ui/")
	}
	restSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           telemetry.HTTPMiddleware(root),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 2)
	go func() {
		slog.Info("HTTP listening", "addr", cfg.HTTPAddr, "rest", "/api", "mcp", "/mcp", "ui", "/ui/")
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
		driver := cfg.StorageDriver
		if strings.TrimSpace(cfg.PostgresDSN) != "" && (driver == "" || driver == "sqlite") && os.Getenv("POSTRA_STORAGE_DRIVER") != "sqlite" {
			driver = "postgres"
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

			var nb int
			if driver == "postgres" {
				store, err := pgstore.Open(context.Background(), cfg.PostgresDSN)
				if err != nil {
					return err
				}
				defer store.Close()
				store.EnableEncryption(kek)
				if nb, err = store.RewrapBodies(context.Background()); err != nil {
					return fmt.Errorf("rewrap bodies: %w", err)
				}
			} else {
				store, err := persistence.Open(filepath.Join(cfg.DataDir, "postra.db"))
				if err != nil {
					return err
				}
				defer store.Close()
				store.EnableEncryption(kek)
				if nb, err = store.RewrapBodies(context.Background()); err != nil {
					return fmt.Errorf("rewrap bodies: %w", err)
				}
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
	inbound := fs.String("inbound-protocol", "pop3", "pop3 | imap (inbound fetch protocol)")
	pop3Host := fs.String("pop3-host", "", "inbound (POP3/IMAP) host")
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
			Name: *name, Email: *email, InboundProtocol: *inbound,
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

// rulesCmd manages mail automation rules from the CLI. Rule creation takes a
// JSON definition (file or stdin) since conditions/actions are structured.
func rulesCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: postra rules list|apply|delete|add ...")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("rules "+sub, flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")
	message := fs.String("message", "", "message ID (for apply)")
	id := fs.String("id", "", "rule ID (for delete)")
	file := fs.String("file", "", "rule JSON file (for add; '-' reads stdin)")
	fs.Parse(rest)

	app, _, err := loadApp(*configPath)
	if err != nil {
		return err
	}
	defer app.Shutdown()
	ctx := application.WithActor(context.Background(), "cli")

	switch sub {
	case "list":
		rules, err := app.ListRules(ctx)
		if err != nil {
			return err
		}
		printJSON(map[string]any{"rules": rules})
		return nil
	case "apply":
		if *message == "" {
			return errors.New("--message is required")
		}
		res, err := app.ApplyRulesToMessage(ctx, *message)
		if err != nil {
			return err
		}
		printJSON(res)
		return nil
	case "delete":
		if *id == "" {
			return errors.New("--id is required")
		}
		if err := app.DeleteRule(ctx, *id); err != nil {
			return err
		}
		fmt.Println("deleted:", *id)
		return nil
	case "add":
		if *file == "" {
			return errors.New("--file is required (a rule JSON, or '-' for stdin)")
		}
		var data []byte
		if *file == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(*file) // #nosec G304 -- operator-supplied path
		}
		if err != nil {
			return err
		}
		var rule domain.MailRule
		if err := json.Unmarshal(data, &rule); err != nil {
			return fmt.Errorf("parse rule JSON: %w", err)
		}
		created, err := app.CreateRule(ctx, rule)
		if err != nil {
			return err
		}
		printJSON(created)
		return nil
	}
	return fmt.Errorf("unknown rules subcommand %q", sub)
}

// cardsCmd manages action cards from the CLI.
func cardsCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: postra cards extract|list|status|export ...")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("cards "+sub, flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")
	message := fs.String("message", "", "message ID (for extract)")
	id := fs.String("id", "", "card ID (for status/export)")
	status := fs.String("status", "", "status filter (list) or new status")
	target := fs.String("target", "", "export target (calendar|jira|itsm)")
	externalRef := fs.String("external-ref", "", "external record id (for export)")
	limit := fs.Int("limit", 100, "max cards (list)")
	fs.Parse(rest)

	app, _, err := loadApp(*configPath)
	if err != nil {
		return err
	}
	defer app.Shutdown()
	ctx := application.WithActor(context.Background(), "cli")

	switch sub {
	case "extract":
		if *message == "" {
			return errors.New("--message is required")
		}
		cards, err := app.ExtractActionCards(ctx, *message)
		if err != nil {
			return err
		}
		printJSON(map[string]any{"cards": cards, "count": len(cards)})
		return nil
	case "list":
		cards, err := app.ListActionCards(ctx, *status, *limit)
		if err != nil {
			return err
		}
		printJSON(map[string]any{"cards": cards})
		return nil
	case "status":
		if *id == "" || *status == "" {
			return errors.New("--id and --status are required")
		}
		card, err := app.SetActionCardStatus(ctx, *id, *status)
		if err != nil {
			return err
		}
		printJSON(card)
		return nil
	case "export":
		if *id == "" || *target == "" {
			return errors.New("--id and --target are required")
		}
		exp, err := app.ExportActionCard(ctx, *id, *target, *externalRef)
		if err != nil {
			return err
		}
		printJSON(exp)
		return nil
	}
	return fmt.Errorf("unknown cards subcommand %q", sub)
}

// teamCmd manages shared-mailbox collaboration from the CLI.
func teamCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: postra team inbox|get|assign|status|note ...")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("team "+sub, flag.ExitOnError)
	configPath := fs.String("config", "", "config file path")
	message := fs.String("message", "", "message ID")
	assignee := fs.String("assignee", "", "assignee (assign) or filter (inbox)")
	status := fs.String("status", "", "status (status set / inbox filter)")
	body := fs.String("body", "", "note body")
	limit := fs.Int("limit", 100, "max items (inbox)")
	fs.Parse(rest)

	app, _, err := loadApp(*configPath)
	if err != nil {
		return err
	}
	defer app.Shutdown()
	ctx := application.WithActor(context.Background(), "cli")

	switch sub {
	case "inbox":
		items, err := app.TeamInbox(ctx, *status, *assignee, *limit)
		if err != nil {
			return err
		}
		printJSON(map[string]any{"items": items, "count": len(items)})
		return nil
	case "get":
		if *message == "" {
			return errors.New("--message is required")
		}
		v, err := app.GetMessageCollab(ctx, *message)
		if err != nil {
			return err
		}
		printJSON(v)
		return nil
	case "assign":
		if *message == "" {
			return errors.New("--message is required")
		}
		mc, err := app.AssignMessage(ctx, *message, *assignee)
		if err != nil {
			return err
		}
		printJSON(mc)
		return nil
	case "status":
		if *message == "" || *status == "" {
			return errors.New("--message and --status are required")
		}
		mc, err := app.SetMessageWorkStatus(ctx, *message, *status)
		if err != nil {
			return err
		}
		printJSON(mc)
		return nil
	case "note":
		if *message == "" || *body == "" {
			return errors.New("--message and --body are required")
		}
		n, err := app.AddMessageNote(ctx, *message, *body)
		if err != nil {
			return err
		}
		printJSON(n)
		return nil
	}
	return fmt.Errorf("unknown team subcommand %q", sub)
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
