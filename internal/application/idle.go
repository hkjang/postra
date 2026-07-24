package application

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"postra/internal/domain"
)

// RunIdleWorker maintains RFC 2177 IMAP IDLE connections for active IMAP
// accounts so newly arrived mail is synced in near real time instead of only
// on the next scheduler tick (§P1 IMAP IDLE). Only the leader runs it; on loss
// of leadership every idle connection is torn down. Runs until ctx is
// cancelled.
func (a *App) RunIdleWorker(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	managed := map[string]context.CancelFunc{} // accountID -> stop
	stopAll := func() {
		for id, cancel := range managed {
			cancel()
			delete(managed, id)
		}
	}
	defer stopAll()

	reconcile := func() {
		if !a.IsLeader() {
			stopAll()
			return
		}
		want := a.activeIMAPAccounts(ctx)
		wanted := map[string]bool{}
		for _, acc := range want {
			wanted[acc.ID] = true
			if _, running := managed[acc.ID]; running {
				continue
			}
			accCtx, cancel := context.WithCancel(ctx)
			managed[acc.ID] = cancel
			accCopy := acc
			a.workerGroup.Add(1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("idle worker panic", "account", accCopy.ID, "panic", r)
						a.recordIncident(domain.SeverityCritical, "idle-worker",
							fmt.Sprintf("panic: %v", r), string(debug.Stack()),
							withIncidentAccount(accCopy.ID))
					}
				}()
				defer a.workerGroup.Done()
				a.idleLoop(accCtx, accCopy)
			}()
			slog.Info("idle worker: watching IMAP account", "account", acc.ID)
		}
		for id, cancel := range managed {
			if !wanted[id] {
				cancel()
				delete(managed, id)
				slog.Info("idle worker: stopped watching account", "account", id)
			}
		}
	}

	a.guard("idle-reconcile", reconcile)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.guard("idle-reconcile", reconcile)
		}
	}
}

// activeIMAPAccounts enumerates active IMAP accounts across all active users.
func (a *App) activeIMAPAccounts(ctx context.Context) []domain.MailAccount {
	sctx := WithActor(ctx, "idle-worker")
	users, err := a.Store.ListUsers(sctx)
	if err != nil {
		slog.Error("idle worker: list users failed", "err", err)
		return nil
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
			continue
		}
		for _, acc := range accts {
			if acc.Status == domain.AccountActive && acc.InboundProtocol == domain.InboundIMAP && acc.POP3Host != "" {
				out = append(out, acc)
			}
		}
	}
	return out
}

// idleLoop keeps one IMAP account under IDLE, reconnecting with capped
// exponential backoff after faults, until ctx is cancelled or leadership is
// lost.
func (a *App) idleLoop(ctx context.Context, acc domain.MailAccount) {
	const baseBackoff = 5 * time.Second
	const maxBackoff = 2 * time.Minute
	backoff := baseBackoff
	for {
		if ctx.Err() != nil || !a.IsLeader() {
			return
		}
		err := a.idleOnce(ctx, &acc)
		if ctx.Err() != nil || !a.IsLeader() {
			return
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

// idleOnce opens one inbound session and idles until an event or fault, kicking
// a sync on connect (to catch anything missed while disconnected) and on every
// activity notification.
func (a *App) idleOnce(ctx context.Context, acc *domain.MailAccount) error {
	sess, err := a.dialInbound(ctx, acc, domain.PurposePOP3Auth)
	if err != nil {
		return err
	}
	defer sess.Close()

	idler, ok := sess.(domain.IdleCapable)
	if !ok {
		return userErrf("inbound session for account %s does not support IDLE", acc.ID)
	}

	// Reconnected: sync anything that arrived while we were down.
	a.triggerIdleSync(ctx, acc)

	for {
		if ctx.Err() != nil || !a.IsLeader() {
			return ctx.Err()
		}
		if err := idler.Idle(ctx); err != nil {
			return err
		}
		a.triggerIdleSync(ctx, acc)
	}
}

// triggerIdleSync launches a best-effort sync for the account. The per-account
// syncLock in StartSync coalesces this with any in-flight sync.
func (a *App) triggerIdleSync(ctx context.Context, acc *domain.MailAccount) {
	uctx := WithPrincipal(WithActor(ctx, "idle-worker"), domain.Principal{
		UserID: acc.UserID, Role: domain.RoleUser, AuthMethod: "idle-worker",
	})
	if _, err := a.StartSync(uctx, acc.ID, SyncOptions{}); err != nil {
		slog.Debug("idle worker: sync skipped", "account", acc.ID, "reason", err)
	}
}
