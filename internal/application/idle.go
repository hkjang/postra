package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"postra/internal/domain"
)

// idleSyncWait bounds how long a watch waits for the sync it handed its
// connection to. A sync that runs longer is fetching a large mailbox, and the
// account is better watched than left unwatched.
var idleSyncWait = 10 * time.Minute

// idleReconcileInterval is how often the supervisor re-reads the account list
// and restarts watches that ended. A leadership change reconciles at once.
var idleReconcileInterval = 60 * time.Second

// idleReconcileTimeout bounds the account listing. The SQLite store serves the
// whole process through one connection, so an unbounded read queued behind a
// long write froze the supervisor, and with it every restart.
const idleReconcileTimeout = 30 * time.Second

// idleWatch is one running IDLE loop. done is closed when the loop's goroutine
// exits for any reason, which is how the supervisor tells a live watch from a
// dead one; settings fingerprints the connection the loop was started with.
type idleWatch struct {
	cancel   context.CancelFunc
	done     chan struct{}
	settings string
}

// idleSettings fingerprints what an IDLE session connects with. The loop holds
// a copy of the account taken at start, so a change here must restart it. The
// row's updated_at is no use: sync status changes bump it too.
func idleSettings(acc domain.MailAccount) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%s|%t", acc.InboundProtocol, acc.POP3Host, acc.POP3Port, acc.POP3Security,
		acc.POP3Username, acc.POP3Secret, acc.InsecureSkipVerify)
}

// RunIdleWorker maintains RFC 2177 IMAP IDLE connections for active IMAP
// accounts so newly arrived mail is synced in near real time instead of only
// on the next scheduler tick (§P1 IMAP IDLE). Only the leader runs it; on loss
// of leadership every idle connection is torn down. Runs until ctx is
// cancelled.
//
// The supervisor owns the set of watches and is the only goroutine that
// touches it. It reconciles on a timer and on every leadership edge, and it
// treats a watch whose goroutine has ended as absent. Before, an entry stayed
// "running" after its loop returned — on a panic, or when the loop saw a
// standby blip shorter than the reconcile interval and quit — so that account
// was never watched again until the process restarted.
func (a *App) RunIdleWorker(ctx context.Context) {
	ticker := time.NewTicker(idleReconcileInterval)
	defer ticker.Stop()

	managed := map[string]idleWatch{}
	stop := func(id string) {
		managed[id].cancel()
		delete(managed, id)
	}
	stopAll := func() {
		for id := range managed {
			stop(id)
		}
	}
	defer stopAll()

	start := func(acc domain.MailAccount) {
		accCtx, cancel := context.WithCancel(ctx)
		w := idleWatch{cancel: cancel, done: make(chan struct{}), settings: idleSettings(acc)}
		managed[acc.ID] = w
		a.workerGroup.Add(1)
		go func() {
			defer a.workerGroup.Done()
			defer close(w.done)
			// A loop that ends on its own (a panic, a standby blip) must still
			// release its context, or each restart leaves a child registered
			// on the worker's context. cancel is idempotent with stop.
			defer cancel()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("idle worker panic", "account", acc.ID, "panic", r)
					a.recordIncident(domain.SeverityCritical, "idle-worker",
						fmt.Sprintf("panic: %v", r), string(debug.Stack()),
						withIncidentAccount(acc.ID))
				}
			}()
			a.idleLoop(accCtx, acc)
		}()
		slog.Info("idle worker: watching IMAP account", "account", acc.ID)
	}

	reconcile := func() {
		// A server that allows only one IMAP connection per account cannot host
		// a permanent watch next to a sync, so operators can switch the watch
		// off and fall back to scheduled syncs.
		if !a.IsLeader() || !a.SettingBool(SettingMailIdleEnabled) {
			stopAll()
			return
		}
		lctx, cancel := context.WithTimeout(ctx, idleReconcileTimeout)
		defer cancel()
		want, err := a.activeIMAPAccounts(lctx)
		if err != nil {
			// An unreadable account list is not an empty one: keep every
			// running watch and try again next time. Returning nil here used
			// to tear down all IDLE connections on one transient DB error.
			slog.Warn("idle worker: account list unavailable; keeping current watches", "err", err, "watching", len(managed))
			return
		}
		wanted := map[string]bool{}
		for _, acc := range want {
			wanted[acc.ID] = true
			if w, running := managed[acc.ID]; running {
				select {
				case <-w.done:
					// Restarted on the timer, not at once: a loop that
					// panics on connect must not spin.
					slog.Warn("idle worker: watch ended; restarting", "account", acc.ID)
					stop(acc.ID)
				default:
					if w.settings == idleSettings(acc) {
						continue
					}
					slog.Info("idle worker: account connection settings changed; reconnecting", "account", acc.ID)
					stop(acc.ID)
				}
			}
			start(acc)
		}
		for id := range managed {
			if !wanted[id] {
				stop(id)
				slog.Info("idle worker: stopped watching account", "account", id)
			}
		}
	}

	for {
		// Taken before reconciling, so an edge during reconcile is not lost.
		edge := a.leaderEdge()
		a.guard("idle-reconcile", reconcile)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-edge:
		}
	}
}

// activeIMAPAccounts enumerates active IMAP accounts across all active users.
// Any listing failure fails the whole enumeration: a partial list would stop
// the watches of every user whose accounts could not be read.
func (a *App) activeIMAPAccounts(ctx context.Context) ([]domain.MailAccount, error) {
	sctx := WithActor(ctx, "idle-worker")
	users, err := a.Store.ListUsers(sctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	var out []domain.MailAccount
	for _, u := range users {
		if u.Status != domain.UserActive {
			continue
		}
		uctx := WithPrincipal(sctx, domain.Principal{
			UserID: u.ID, LoginID: u.LoginID, Role: u.Role, AuthMethod: "idle-worker",
		})
		accts, err := a.Store.ListAccounts(uctx, u.ID)
		if err != nil {
			return nil, fmt.Errorf("list accounts of user %s: %w", u.ID, err)
		}
		for _, acc := range accts {
			if acc.Status == domain.AccountActive && acc.InboundProtocol == domain.InboundIMAP && acc.POP3Host != "" {
				out = append(out, acc)
			}
		}
	}
	return out, nil
}

// idleLoop keeps one IMAP account under IDLE, reconnecting with capped
// exponential backoff after faults, until ctx is cancelled or leadership is
// lost.
func (a *App) idleLoop(ctx context.Context, acc domain.MailAccount) {
	const baseBackoff = 5 * time.Second
	const maxBackoff = 2 * time.Minute
	backoff := baseBackoff
	seen := -1 // no connection yet
	for {
		if ctx.Err() != nil || !a.IsLeader() {
			return
		}
		count, err := a.idleOnce(ctx, &acc, seen)
		seen = count
		if ctx.Err() != nil || !a.IsLeader() {
			return
		}
		if errors.Is(err, errIdleYielded) {
			// The connection went to a sync that has now finished; take it back
			// immediately. This is the normal path, not a fault.
			backoff = baseBackoff
			continue
		}
		if err != nil {
			slog.Debug("idle worker: session error, will reconnect", "account", acc.ID, "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = baseBackoff
	}
}

// errIdleYielded ends a watch that handed its connection to a sync. It is not
// a fault: the loop reconnects at once, without backoff.
var errIdleYielded = errors.New("idle watch yielded its connection to a sync")

// idleOnce opens one inbound session and idles until an event or fault.
//
// A mail server counts connections, and many allow only one or two per
// account, answering the next one by accepting the TCP connection and then
// never greeting it. So the watch does not hold its connection while the sync
// it triggered runs: it closes the session, waits for that sync, and returns
// errIdleYielded so the loop reconnects. An account therefore uses one inbound
// connection at a time, not two.
//
// seen is the INBOX message count this watch last saw; a negative value means
// it has not connected yet. Because nothing here deletes server-side, a higher
// count means mail arrived while the connection was away — the one thing a
// watch would otherwise miss by yielding — and only then is a sync started on
// connect. A server that reports no count syncs on the first connection only.
func (a *App) idleOnce(ctx context.Context, acc *domain.MailAccount, seen int) (count int, err error) {
	sess, err := a.dialInbound(ctx, acc, domain.PurposePOP3Auth)
	if err != nil {
		return seen, err
	}
	defer sess.Close()

	idler, ok := sess.(domain.IdleCapable)
	if !ok {
		return seen, userErrf("inbound session for account %s does not support IDLE", acc.ID)
	}
	count = 0
	if counter, ok := sess.(domain.MailboxCounter); ok {
		count = counter.MessageCount()
	}

	// Freshly connected: sync what arrived while no watch was listening. After
	// a yield the sync has just run, so this repeats only when the mailbox grew
	// during it — never as a loop of its own.
	if seen < 0 || count > seen {
		sess.Close()
		a.runIdleSync(ctx, acc)
		return count, errIdleYielded
	}

	for {
		if ctx.Err() != nil || !a.IsLeader() {
			return count, ctx.Err()
		}
		if err := idler.Idle(ctx); err != nil {
			return count, err
		}
		// Activity: hand the account's connection to the sync.
		sess.Close()
		a.runIdleSync(ctx, acc)
		return count, errIdleYielded
	}
}

// runIdleSync starts a sync for the account and waits for it, so the watch does
// not reconnect while that sync still holds the account's server connection.
// The per-account syncLock in StartSync coalesces this with any in-flight sync;
// when it is already running, this returns at once and the watch reconnects —
// the running sync ends on its own.
func (a *App) runIdleSync(ctx context.Context, acc *domain.MailAccount) {
	uctx := WithPrincipal(WithActor(ctx, "idle-worker"), domain.Principal{
		UserID: acc.UserID, Role: domain.RoleUser, AuthMethod: "idle-worker",
	})
	done := make(chan struct{})
	if _, err := a.StartSync(uctx, acc.ID, SyncOptions{fromIdle: true, done: done}); err != nil {
		slog.Debug("idle worker: sync skipped", "account", acc.ID, "reason", err)
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(idleSyncWait):
		// The sync outlives this wait only if it is fetching a great deal;
		// reconnecting then is better than leaving the account unwatched.
		slog.Warn("idle worker: sync still running; resuming the watch", "account", acc.ID, "waited", idleSyncWait)
	}
}
