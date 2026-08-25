package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"postra/internal/domain"
)

// digestMaxMessages bounds how much mail one briefing considers, keeping the
// prompt within budget on heavy days.
const digestMaxMessages = 60

// GenerateDailyDigest summarizes the mail received over the last `hours` into
// a short briefing: what needs a reply, what has a deadline, and what is just
// FYI. Answering "what happened in my inbox today" otherwise means reading
// everything.
func (a *App) GenerateDailyDigest(ctx context.Context, accountID string, hours int) (*domain.Analysis, error) {
	if hours <= 0 {
		hours = 24
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	res, err := a.Store.Search(ctx, domain.SearchQuery{
		UserID: userIDFrom(ctx), AccountID: accountID, Since: since, Limit: digestMaxMessages,
	})
	if err != nil {
		return nil, err
	}
	if len(res.Messages) == 0 {
		return nil, userErrf("최근 %d시간 동안 브리핑할 메일이 없습니다", hours)
	}
	var sb strings.Builder
	for _, m := range res.Messages {
		body, _ := a.Store.GetBody(ctx, userIDFrom(ctx), m.ID)
		text := ""
		if body != nil {
			text = truncateRunes(body.TextBody, 800)
		}
		fmt.Fprintf(&sb, "[%s] Subject: %s | From: %s | Date: %s\n%s\n\n",
			m.ID, m.Subject, m.From.Email, fmtUnix(m.Date), text)
	}
	return a.runAnalysis(ctx, "daily_digest", "digest",
		fmt.Sprintf("%dh", hours), fmt.Sprintf("Period: last %d hours, %d messages.", hours, len(res.Messages)), sb.String())
}

// digestStateKey records the last date a user's briefing was produced, so the
// worker fires once per day even across restarts and replica failover.
const digestStateKey = "internal.last_digest."

// RunDigestWorker produces each active user's daily briefing at the configured
// hour. Leader-only and idempotent per day.
func (a *App) RunDigestWorker(ctx context.Context) {
	if !a.Cfg.Sync.DailyDigestEnabled {
		slog.Info("daily digest disabled (sync.daily_digest_enabled = false)")
		return
	}
	slog.Info("daily digest worker started", "hour", a.Cfg.Sync.DailyDigestHour)
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.IsLeader() {
				a.guard("digest-worker", func() { a.digestOnce(ctx) })
			}
		}
	}
}

// digestOnce generates today's briefing for every active user that has not had
// one yet, once the configured hour has arrived.
func (a *App) digestOnce(ctx context.Context) int {
	now := time.Now()
	if now.Hour() < a.Cfg.Sync.DailyDigestHour {
		return 0
	}
	today := now.Format("2006-01-02")
	if err := a.checkAIPolicy(ctx); err != nil {
		slog.Debug("digest worker: skipped by AI policy", "err", err)
		return 0
	}
	settings, err := a.Store.GetSettings(ctx)
	if err != nil {
		return 0
	}
	users, err := a.Store.ListUsers(WithActor(ctx, "digest-worker"))
	if err != nil {
		return 0
	}
	made := 0
	for _, u := range users {
		if u.Status != domain.UserActive {
			continue
		}
		key := digestStateKey + u.ID
		if settings[key] == today {
			continue // already briefed today
		}
		uctx := WithPrincipal(WithActor(ctx, "digest-worker"), domain.Principal{
			UserID: u.ID, LoginID: u.LoginID, Role: u.Role, AuthMethod: "digest-worker",
		})
		if _, err := a.GenerateDailyDigest(uctx, "", 24); err != nil {
			var ue *UserError
			if !errors.As(err, &ue) {
				// A provider or storage fault is transient. Marking the day
				// done here would cost the user their entire briefing over a
				// momentary outage, so leave it for the next tick.
				slog.Warn("digest worker: briefing failed, will retry", "user", u.ID, "err", err)
				continue
			}
			// A UserError means there is genuinely nothing to brief today
			// (e.g. no mail arrived); marking the day avoids retrying every tick.
			slog.Debug("digest worker: nothing to brief", "user", u.ID, "err", err)
		}
		_ = a.Store.UpsertSettings(ctx, map[string]string{key: today})
		made++
	}
	if made > 0 {
		slog.Info("daily digest worker: briefings produced", "count", made)
	}
	return made
}
