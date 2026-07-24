// Package application holds the use-case layer shared by every transport.
// REST handlers, MCP tools, and the CLI all call into App — none of them
// touch adapters or the DB directly (§16).
package application

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

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

	syncLocks    sync.Map // accountID -> struct{} (best-effort single-session lock, POP-003)
	jobCancels   sync.Map // jobID -> context.CancelFunc
	background   context.Context
	cancelAll    context.CancelFunc
	workerGroup  sync.WaitGroup
	oidcStateKey [32]byte
	aiConfigMu   sync.RWMutex
	aiRaw        domain.AIProvider
	vectorStore  VectorStore
	vectorMu     sync.RWMutex
	nodeID       string
	leaderMu     sync.RWMutex
	isLeader     bool
	mcpPolicy    mcpPolicyState
	syncSem      chan struct{} // bounds concurrent account syncs (OOM guard)
}

func New(cfg config.Config, store Storage, objects objectstore.Store,
	secrets domain.SecretStore, pop3 domain.POP3Dialer, smtp domain.SMTPClient, ai domain.AIProvider) (*App, error) {
	if stored, err := store.GetSettings(context.Background()); err == nil {
		applyStoredSettings(&cfg, stored)
	}
	bg, cancel := context.WithCancel(context.Background())
	syncConcurrency := cfg.Sync.MaxConcurrentSyncs
	if syncConcurrency <= 0 {
		syncConcurrency = 2
	}
	a := &App{
		Cfg: cfg, Store: store, Objects: objects, Secrets: secrets,
		POP3: pop3, SMTP: smtp, AI: meteredAI{inner: ai}, aiRaw: ai,
		Scanner:    malware.NewHeuristic(cfg.Attachments),
		background: bg, cancelAll: cancel,
		syncSem:    make(chan struct{}, syncConcurrency),
	}
	candidate := make([]byte, len(a.oidcStateKey))
	if _, err := rand.Read(candidate); err != nil {
		cancel()
		return nil, err
	}
	sharedKey, err := store.GetOrCreateSetting(context.Background(), "internal.oidc_state_key",
		base64.RawURLEncoding.EncodeToString(candidate))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("initialize shared OIDC state key: %w", err)
	}
	decodedKey, err := base64.RawURLEncoding.DecodeString(sharedKey)
	if err != nil || len(decodedKey) != len(a.oidcStateKey) {
		cancel()
		return nil, fmt.Errorf("invalid shared OIDC state key")
	}
	copy(a.oidcStateKey[:], decodedKey)
	if err := store.EnsureUser(context.Background(), DefaultUserID, "local"); err != nil {
		return nil, err
	}
	if err := a.bootstrapAdmin(context.Background()); err != nil {
		return nil, err
	}
	a.initVectorStore(context.Background())
	a.loadMCPPolicy(context.Background())

	// Start listening for settings changes if supported by the store (multi-node sync)
	if notifier, ok := store.(interface {
		ListenSettingsChange(ctx context.Context, cb func())
	}); ok {
		notifier.ListenSettingsChange(a.background, func() {
			slog.Info("app: settings change notification received, re-initializing configuration and vector store")
			if stored, err := a.Store.GetSettings(context.Background()); err == nil {
				a.applyAISettings(stored)
			}
			a.initVectorStore(context.Background())
			a.loadMCPPolicy(context.Background())
		})
	}

	a.nodeID = "node_" + randomToken(10)
	// Leader election is NOT started here. Only the `serve` worker process
	// should contend for the lease (via StartLeaderElection); short-lived CLI
	// commands and the long-lived `postra mcp` stdio process must never grab
	// leadership, or they would starve the real worker (P0 리더 선출 시작 위치).

	return a, nil
}

// Ready reports whether the app's backing store is reachable, for readiness
// probes. Liveness (process is up) needs no dependency check.
func (a *App) Ready(ctx context.Context) error {
	return a.Store.Ping(ctx)
}

// Shutdown cancels background jobs and waits for workers to drain.
func (a *App) Shutdown() {
	a.cancelAll()
	a.workerGroup.Wait()
	a.vectorMu.Lock()
	if a.vectorStore != nil {
		_ = a.vectorStore.Close()
	}
	a.vectorMu.Unlock()
}

// ---------- actor context ----------

type ctxKey int

const (
	actorKey     ctxKey = 1
	principalKey ctxKey = 2
)

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

func WithPrincipal(ctx context.Context, p domain.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func PrincipalFrom(ctx context.Context) (domain.Principal, bool) {
	p, ok := ctx.Value(principalKey).(domain.Principal)
	return p, ok
}

func userIDFrom(ctx context.Context) string {
	if p, ok := PrincipalFrom(ctx); ok && p.UserID != "" {
		return p.UserID
	}
	return DefaultUserID
}

func (a *App) audit(ctx context.Context, action, resource, result, detail string) {
	_ = a.Store.AppendAudit(ctx, domain.AuditEvent{
		UserID: userIDFrom(ctx), Actor: actorFrom(ctx),
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
		MaxMessageBytes:    a.Cfg.Sync.MaxMessageBytes,
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

func (a *App) initVectorStore(ctx context.Context) {
	a.vectorMu.Lock()
	defer a.vectorMu.Unlock()

	if a.vectorStore != nil {
		_ = a.vectorStore.Close()
	}

	settings, err := a.Store.GetSettings(ctx)
	if err != nil {
		slog.Error("app: failed to load settings for vector store initialization", "error", err)
		a.vectorStore = &DisabledVectorStore{}
		return
	}

	provider := settings[SettingVectorProvider]
	if provider == "" {
		// Default fallback
		if pg, ok := a.Store.(interface{ HasPgVector() bool }); ok {
			if pg.HasPgVector() {
				provider = "postgres"
			} else {
				provider = "disabled"
				slog.Warn("app: pgvector is not available and no alternative vector provider is configured. Semantic search is disabled.")
			}
		} else {
			provider = "sqlite"
		}
	}

	slog.Info("app: initializing vector store provider", "provider", provider)

	switch provider {
	case "postgres", "sqlite":
		if provider == "postgres" {
			if pg, ok := a.Store.(interface{ HasPgVector() bool }); ok && !pg.HasPgVector() {
				slog.Error("app: requested postgres vector provider, but pgvector is not available. Falling back to disabled vector store.")
				a.vectorStore = &DisabledVectorStore{}
				return
			}
		}
		a.vectorStore = &StorageVectorStore{store: a.Store}
	case "milvus":
		url := settings[SettingVectorMilvusURL]
		collection := settings[SettingVectorMilvusCollection]
		if url == "" {
			slog.Error("app: milvus provider configured but vector.milvus_url is empty. Falling back to disabled vector store.")
			a.vectorStore = &DisabledVectorStore{}
			return
		}
		token := a.resolveMilvusToken(ctx, settings)
		a.vectorStore = NewMilvusVectorStore(url, token, collection, a.Store)
	default:
		a.vectorStore = &DisabledVectorStore{}
	}
}

// resolveMilvusToken fetches the Milvus token from the encrypted SecretStore
// via its reference, falling back to a legacy plaintext setting for
// deployments that predate the secret-ref migration.
func (a *App) resolveMilvusToken(ctx context.Context, settings map[string]string) string {
	if ref := settings[SettingVectorMilvusTokenRef]; ref != "" {
		h, err := a.Secrets.Acquire(ctx, domain.SecretRef(ref), domain.PurposeVectorToken)
		if err != nil {
			slog.Error("app: failed to acquire milvus token secret", "error", err)
			return ""
		}
		token := string(h.Reveal())
		h.Zero()
		return token
	}
	if legacy := settings[SettingVectorMilvusToken]; legacy != "" {
		slog.Warn("app: using legacy plaintext vector.milvus_token; re-save Milvus settings to migrate it into the encrypted secret store")
		return legacy
	}
	return ""
}

func (a *App) VectorStore() VectorStore {
	a.vectorMu.RLock()
	defer a.vectorMu.RUnlock()
	return a.vectorStore
}

func (a *App) IsLeader() bool {
	a.leaderMu.RLock()
	defer a.leaderMu.RUnlock()
	return a.isLeader
}

func (a *App) setLeaderState(state bool) {
	a.leaderMu.Lock()
	rising := state && !a.isLeader
	if a.isLeader != state {
		a.isLeader = state
		if state {
			slog.Info("app: this node has been elected as LEADER. Running background tasks.", "node_id", a.nodeID)
		} else {
			slog.Info("app: this node is now STANDBY. Pausing background tasks.", "node_id", a.nodeID)
		}
	}
	a.leaderMu.Unlock()
	// On the standby→leader rising edge, recover jobs a crashed/demoted leader
	// left mid-flight. Doing it here (not only at scheduler start) covers a node
	// that started as standby and was later promoted, and it runs even when
	// auto-sync is disabled (P1 장애 복구 시점).
	if rising {
		a.onBecameLeader()
	}
}

// onBecameLeader runs one-shot recovery when this node acquires leadership.
func (a *App) onBecameLeader() {
	a.workerGroup.Add(1)
	go func() {
		defer a.workerGroup.Done()
		ctx := WithActor(a.background, "scheduler")
		a.RecoverStaleJobs(ctx)
	}()
}

// StartLeaderElection begins the background leader-election loop. Call this
// exactly once, and only from the `serve` worker process.
func (a *App) StartLeaderElection() {
	a.startLeaderElectionLoop()
}

func (a *App) startLeaderElectionLoop() {
	a.workerGroup.Add(1)
	go func() {
		defer a.workerGroup.Done()

		a.electLeader(context.Background())

		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-a.background.Done():
				a.releaseLease(context.Background())
				return
			case <-ticker.C:
				a.electLeader(context.Background())
			}
		}
	}()
}

func (a *App) ActiveJobIDs() []string {
	var ids []string
	a.jobCancels.Range(func(key, value any) bool {
		if id, ok := key.(string); ok {
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

func (a *App) electLeader(ctx context.Context) {
	if !a.Cfg.WorkerEnabled {
		a.setLeaderState(false)
		return
	}

	const leaseKey = "internal.leader_lease"
	const leaseSec = 15

	acquired, err := a.Store.TryAcquireLease(ctx, leaseKey, a.nodeID, leaseSec)
	if err != nil {
		slog.Error("app: leader election check failed", "error", err, "node_id", a.nodeID)
		return
	}
	a.setLeaderState(acquired)
}

func (a *App) releaseLease(ctx context.Context) {
	a.leaderMu.RLock()
	isL := a.isLeader
	a.leaderMu.RUnlock()
	if !isL {
		return
	}

	const leaseKey = "internal.leader_lease"

	type Lease struct {
		NodeID    string `json:"node_id"`
		ExpiresAt int64  `json:"expires_at"`
	}
	newLease := Lease{NodeID: a.nodeID, ExpiresAt: time.Now().Unix() - 1}
	newVal, _ := json.Marshal(newLease)

	slog.Info("app: releasing leader lease on shutdown", "node_id", a.nodeID)
	_ = a.Store.UpsertSettings(ctx, map[string]string{
		leaseKey: string(newVal),
	})
	a.setLeaderState(false)
}
