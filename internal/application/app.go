// Package application holds the use-case layer shared by every transport.
// REST handlers, MCP tools, and the CLI all call into App — none of them
// touch adapters or the DB directly (§16).
package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"

	"postra/internal/adapters/malware"
	"postra/internal/adapters/objectstore"
	"postra/internal/domain"
	"postra/internal/platform/config"
)

// DefaultUserID is the single-user MVP principal; the schema is already
// multi-user so adding real authentication later is additive.
const DefaultUserID = "usr_local"

type App struct {
	Cfg     config.Config
	Store   Storage
	Objects objectstore.Store
	Secrets domain.SecretStore
	POP3    domain.POP3Dialer
	IMAP    domain.InboundDialer // optional; used when an account's InboundProtocol is "imap"
	SMTP    domain.SMTPClient
	AI      domain.AIProvider
	Scanner domain.AttachmentScanner

	syncLocks   sync.Map // accountID -> struct{} (best-effort single-session lock, POP-003)
	jobCancels  sync.Map // jobID -> context.CancelFunc
	background  context.Context
	cancelAll   context.CancelFunc
	workerGroup sync.WaitGroup
}

func New(cfg config.Config, store Storage, objects objectstore.Store,
	secrets domain.SecretStore, pop3 domain.POP3Dialer, smtp domain.SMTPClient, ai domain.AIProvider) (*App, error) {
	bg, cancel := context.WithCancel(context.Background())
	a := &App{
		Cfg: cfg, Store: store, Objects: objects, Secrets: secrets,
		POP3: pop3, SMTP: smtp, AI: meteredAI{inner: ai},
		Scanner:    malware.NewHeuristic(cfg.Attachments),
		background: bg, cancelAll: cancel,
	}
	if err := store.EnsureUser(context.Background(), DefaultUserID, "local"); err != nil {
		return nil, err
	}
	return a, nil
}

// Shutdown cancels background jobs and waits for workers to drain.
func (a *App) Shutdown() {
	a.cancelAll()
	a.workerGroup.Wait()
}

// ---------- actor context ----------

type ctxKey int

const actorKey ctxKey = 1

// WithActor tags a context with the calling transport ("rest", "mcp", "cli",
// "worker") for audit records.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey, actor)
}

func actorFrom(ctx context.Context) string {
	if v, ok := ctx.Value(actorKey).(string); ok {
		return v
	}
	return "unknown"
}

func (a *App) audit(ctx context.Context, action, resource, result, detail string) {
	_ = a.Store.AppendAudit(ctx, domain.AuditEvent{
		UserID: DefaultUserID, Actor: actorFrom(ctx),
		Action: action, Resource: resource, Result: result, Detail: detail,
	})
}

// ---------- policy checks ----------

// UserError marks failures the caller can fix (bad input, policy denial) as
// opposed to system faults — MCP maps these to tool errors (§10.1).
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErrf(format string, args ...any) error {
	return &UserError{Msg: fmt.Sprintf(format, args...)}
}

// validateMailHost enforces ACC-008/009: metadata and link-local ranges are
// always rejected; private/loopback ranges require AllowPrivateHosts
// (default on, since on-prem deployments are a primary target).
func (a *App) validateMailHost(ctx context.Context, host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return userErrf("mail host is empty")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return userErrf("cannot resolve host %q: %v", host, err)
	}
	for _, ip := range ips {
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return userErrf("host %q resolves to a forbidden address (%s)", host, ip)
		}
		if (ip.IsPrivate() || ip.IsLoopback()) && !a.Cfg.AllowPrivateHosts {
			return userErrf("host %q resolves to a private address (%s); enable allow_private_hosts for on-prem servers", host, ip)
		}
	}
	return nil
}

// checkInsecureAllowed gates plaintext transport, missing SMTP AUTH, and
// certificate-verification bypass behind the AllowInsecureMail policy and
// records an audit trail (§4.2 exceptions).
func (a *App) checkInsecureAllowed(ctx context.Context, acc *domain.MailAccount) error {
	insecure := acc.POP3Security == domain.SecurityNone ||
		acc.SMTPSecurity == domain.SecurityNone ||
		acc.SMTPAuth == "none" || acc.InsecureSkipVerify
	if !insecure {
		return nil
	}
	if !a.Cfg.AllowInsecureMail {
		return userErrf("account uses plaintext/unauthenticated mail transport; set allow_insecure_mail=true (offline networks only)")
	}
	a.audit(ctx, "insecure_transport_configured", "account:"+acc.ID, "ok",
		fmt.Sprintf("pop3=%s smtp=%s smtp_auth=%s skip_verify=%v",
			acc.POP3Security, acc.SMTPSecurity, acc.SMTPAuth, acc.InsecureSkipVerify))
	return nil
}

// dialInbound acquires the account's inbound secret (if any), opens a session
// with the protocol-appropriate adapter (POP3 or IMAP), and zeroes the secret
// immediately after the handshake. The handle never leaves this call
// (SEC-KEY-005).
func (a *App) dialInbound(ctx context.Context, acc *domain.MailAccount, purpose domain.SecretPurpose) (domain.InboundSession, error) {
	dialer, err := a.inboundDialer(acc)
	if err != nil {
		return nil, err
	}
	var secret *domain.SecretHandle
	if acc.POP3Secret != "" {
		secret, err = a.Secrets.Acquire(ctx, acc.POP3Secret, purpose)
		if err != nil {
			return nil, err
		}
		a.Store.TouchCredential(ctx, acc.POP3Secret)
	}
	sess, err := dialer.Dial(ctx, domain.InboundDialOptions{
		Host: acc.POP3Host, Port: acc.POP3Port, Security: acc.POP3Security,
		Username: acc.POP3Username, Password: secret,
		InsecureSkipVerify: acc.InsecureSkipVerify,
		ConnectTimeoutSec:  a.Cfg.Sync.ConnectTimeoutSec,
		CommandTimeoutSec:  a.Cfg.Sync.CommandTimeoutSec,
	})
	if secret != nil {
		secret.Zero()
	}
	return sess, err
}

// inboundDialer picks the fetch adapter for the account's protocol. Empty
// protocol means POP3 (accounts created before IMAP support).
func (a *App) inboundDialer(acc *domain.MailAccount) (domain.InboundDialer, error) {
	switch acc.InboundProtocol {
	case "", domain.InboundPOP3:
		return a.POP3, nil
	case domain.InboundIMAP:
		if a.IMAP == nil {
			return nil, userErrf("IMAP adapter is not configured")
		}
		return a.IMAP, nil
	default:
		return nil, userErrf("unknown inbound protocol %q", acc.InboundProtocol)
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
