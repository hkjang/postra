package application

import (
	"context"
	"log/slog"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/metrics"
)

// staleJobGraceSeconds is how long a queued/running job may go without a
// heartbeat (updated_at bump) before recovery treats it as abandoned. It must
// comfortably exceed the sync heartbeat interval so a live worker — on this or
// any other replica — is never mistaken for a dead one during a leader flap.
var staleJobGraceSeconds = 90

// RecoverStaleJobs fails jobs abandoned by a crashed/restarted worker while
// sparing any that are still actively heart-beating. Safe to call repeatedly:
// it runs on the leader-transition edge and periodically via the reaper
// (비기능 "Worker 장애 후 Job 재개").
func (a *App) RecoverStaleJobs(ctx context.Context) {
	activeIDs := a.ActiveJobIDs()
	n, err := a.Store.RecoverStaleJobsExcept(ctx, activeIDs, staleJobGraceSeconds)
	if err != nil {
		slog.Error("recover stale jobs failed", "err", err)
		return
	}
	if n > 0 {
		slog.Info("marked abandoned jobs as failed", "count", n)
		a.audit(WithActor(ctx, "scheduler"), "jobs_recovered", "jobs", "ok", "no heartbeat past grace window")
	}
}

// RunJobReaper periodically reaps jobs whose worker stopped heart-beating,
// catching crashes that happen while this node is already the stable leader
// (the leader-transition edge alone would miss them). Leader-only.
func (a *App) RunJobReaper(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.IsLeader() {
				a.RecoverStaleJobs(ctx)
			}
		}
	}
}

// RunScheduler runs periodic sync of active POP3 accounts until ctx is
// cancelled. The per-account syncLocks in StartSync prevent overlap, so a
// slow account is simply skipped on the next tick instead of stacking.
func (a *App) RunScheduler(ctx context.Context) {
	interval := time.Duration(a.Cfg.Sync.AutoSyncMinutes) * time.Minute
	if interval <= 0 {
		slog.Info("auto-sync disabled (sync.auto_sync_minutes = 0)")
		return
	}
	slog.Info("scheduler started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Job recovery is handled on the leader-transition edge (onBecameLeader),
	// so it runs even when auto-sync is disabled. Here we only kick an initial
	// sync if this node is already the leader.
	if a.IsLeader() {
		a.syncAllActive(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.IsLeader() {
				a.syncAllActive(ctx)
			}
		}
	}
}

// RunRetryWorker drains the outbox retry queue on a fixed cadence until ctx
// is cancelled (SMTP-011). Runs alongside the sync scheduler.
func (a *App) RunRetryWorker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.IsLeader() {
				if n := a.ProcessRetries(ctx); n > 0 {
					slog.Info("outbox retries processed", "count", n)
				}
			}
		}
	}
}

// ProcessRetries attempts every due retry once and returns how many it acted
// on. Exposed for deterministic testing and manual triggering.
func (a *App) ProcessRetries(ctx context.Context) int {
	due, err := a.Store.ListDueRetries(ctx, time.Now().Unix(), 50)
	if err != nil {
		slog.Error("list due retries failed", "err", err)
		return 0
	}
	metrics.OutboxPending.Set(float64(len(due)))
	metrics.SMTPRetries.Add(float64(len(due)))
	wctx := WithActor(ctx, "worker")
	for _, out := range due {
		o := out // copy
		d, v, err := a.Store.GetDraft(wctx, o.UserID, o.DraftID)
		if err != nil {
			_ = a.Store.UpdateOutbound(wctx, o.ID, domain.OutboundFailed, "draft unavailable for retry", o.Attempts)
			continue
		}
		vv, err := a.Store.GetDraftVersion(wctx, o.UserID, o.DraftID, o.DraftVersion)
		if err != nil {
			vv = v // fall back to current version
		}
		acc, err := a.Store.GetAccount(wctx, o.UserID, d.AccountID)
		if err != nil {
			_ = a.Store.UpdateOutbound(wctx, o.ID, domain.OutboundFailed, "account unavailable for retry", o.Attempts)
			continue
		}
		receipt, sendErr := a.deliver(wctx, &o, acc, vv)
		a.applySendResult(wctx, &o, o.DraftID, receipt, sendErr)
	}
	return len(due)
}

func (a *App) syncAllActive(ctx context.Context) {
	sctx := WithActor(ctx, "scheduler")
	users, err := a.Store.ListUsers(sctx)
	if err != nil {
		slog.Error("scheduler: list users failed", "err", err)
		return
	}
	for _, user := range users {
		if user.Status != domain.UserActive {
			continue
		}
		uctx := WithPrincipal(sctx, domain.Principal{
			UserID: user.ID, LoginID: user.LoginID, Role: user.Role, AuthMethod: "scheduler",
		})
		accts, err := a.Store.ListAccounts(uctx, user.ID)
		if err != nil {
			slog.Error("scheduler: list accounts failed", "user", user.ID, "err", err)
			continue
		}
		for _, acc := range accts {
			if acc.Status != domain.AccountActive || acc.POP3Host == "" {
				continue
			}
			if _, err := a.StartSync(uctx, acc.ID, SyncOptions{}); err != nil {
				slog.Debug("scheduler: sync skipped", "account", acc.ID, "reason", err)
			}
		}
	}
}
