package application

import (
	"context"
	"log/slog"
	"time"

	"postra/internal/domain"
)

// RecoverStaleJobs marks jobs interrupted by a restart as failed so they are
// not reported as perpetually running. Called once at startup before the
// scheduler begins (비기능 "Worker 장애 후 Job 재개").
func (a *App) RecoverStaleJobs(ctx context.Context) {
	n, err := a.Store.RecoverStaleJobs(ctx)
	if err != nil {
		slog.Error("recover stale jobs failed", "err", err)
		return
	}
	if n > 0 {
		slog.Info("marked interrupted jobs as failed", "count", n)
		a.audit(WithActor(ctx, "scheduler"), "jobs_recovered", "jobs", "ok", "interrupted on restart")
	}
}

// RunScheduler runs periodic sync of active POP3 accounts until ctx is
// cancelled. The per-account syncLocks in StartSync prevent overlap, so a
// slow account is simply skipped on the next tick instead of stacking.
func (a *App) RunScheduler(ctx context.Context) {
	a.RecoverStaleJobs(ctx)

	interval := time.Duration(a.Cfg.Sync.AutoSyncMinutes) * time.Minute
	if interval <= 0 {
		slog.Info("auto-sync disabled (sync.auto_sync_minutes = 0)")
		return
	}
	slog.Info("scheduler started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	a.syncAllActive(ctx) // run once at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.syncAllActive(ctx)
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
			if n := a.ProcessRetries(ctx); n > 0 {
				slog.Info("outbox retries processed", "count", n)
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
	accts, err := a.Store.ListAccounts(sctx, DefaultUserID)
	if err != nil {
		slog.Error("scheduler: list accounts failed", "err", err)
		return
	}
	for _, acc := range accts {
		if acc.Status != domain.AccountActive || acc.POP3Host == "" {
			continue
		}
		if _, err := a.StartSync(sctx, acc.ID, SyncOptions{}); err != nil {
			// A "already running" skip is expected and benign.
			slog.Debug("scheduler: sync skipped", "account", acc.ID, "reason", err)
		}
	}
}
