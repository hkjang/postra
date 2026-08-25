package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"postra/internal/domain"
)

const (
	// triageBatch bounds AI calls per worker tick per user.
	triageBatch = 20
	// triageLookbackHours limits triage to recently ingested mail, so enabling
	// the feature does not fan out one AI call per message in the archive.
	triageLookbackHours = 72
	// aiLabelPrefix namespaces labels written by AI triage, keeping them
	// visually and programmatically distinct from the user's own labels.
	aiLabelPrefix = "ai/"
)

// RunTriageWorker classifies newly arrived mail so the inbox can be read by
// urgency instead of arrival order. Each message gets an "ai/<priority>" label
// (searchable and filterable with the existing label facet), and urgent/high
// mail is also flagged important. Opt-in, leader-only, bounded per tick.
func (a *App) RunTriageWorker(ctx context.Context) {
	if !a.Cfg.Sync.AutoTriage {
		slog.Info("auto-triage disabled (sync.auto_triage = false)")
		return
	}
	interval := time.Duration(a.Cfg.Sync.AutoTriageMinutes) * time.Minute
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	slog.Info("triage worker started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.IsLeader() {
				a.guard("triage-worker", func() { a.triageOnce(ctx) })
			}
		}
	}
}

// triageOnce triages one bounded batch per active user. Returns how many
// messages were labelled (exposed for tests).
func (a *App) triageOnce(ctx context.Context) int {
	if err := a.checkAIPolicy(ctx); err != nil {
		slog.Debug("triage worker: skipped by AI policy", "err", err)
		return 0
	}
	sctx := WithActor(ctx, "triage-worker")
	users, err := a.Store.ListUsers(sctx)
	if err != nil {
		return 0
	}
	since := time.Now().Add(-triageLookbackHours * time.Hour).Unix()
	total := 0
	for _, u := range users {
		if u.Status != domain.UserActive {
			continue
		}
		uctx := WithPrincipal(sctx, domain.Principal{
			UserID: u.ID, LoginID: u.LoginID, Role: u.Role, AuthMethod: "triage-worker",
		})
		ids, err := a.Store.MessagesNeedingTriage(uctx, u.ID, since, triageBatch)
		if err != nil || len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return total
			}
			if a.triageMessage(uctx, id) {
				total++
			}
		}
	}
	if total > 0 {
		slog.Info("triage worker: classified new mail", "count", total)
	}
	return total
}

// triageMessage runs triage for one message and records the outcome as a
// label. Returns whether the message was labelled.
func (a *App) triageMessage(ctx context.Context, messageID string) bool {
	an, err := a.AnalyzeMessage(ctx, messageID, "triage")
	if err != nil {
		slog.Debug("triage worker: analysis failed", "message", messageID, "err", err)
		return false
	}
	var parsed struct {
		Priority      string `json:"priority"`
		ReplyRequired bool   `json:"reply_required"`
	}
	if err := json.Unmarshal([]byte(an.ResultJSON), &parsed); err != nil {
		return false
	}
	priority := strings.ToLower(strings.TrimSpace(parsed.Priority))
	switch priority {
	case "urgent", "high", "normal", "low":
	default:
		priority = "normal" // never write an unvetted label value
	}
	m, err := a.Store.GetMessage(ctx, userIDFrom(ctx), messageID)
	if err != nil {
		return false
	}
	label := aiLabelPrefix + priority
	for _, l := range m.Labels {
		if l == label {
			return false // already labelled
		}
	}
	m.Labels = append(m.Labels, label)
	if parsed.ReplyRequired {
		m.Labels = append(m.Labels, aiLabelPrefix+"reply-needed")
	}
	// Surface the mail that actually matters through the existing important flag.
	if priority == "urgent" || priority == "high" {
		m.IsImportant = true
	}
	if err := a.Store.UpdateMessage(ctx, m); err != nil {
		slog.Warn("triage worker: label update failed", "message", messageID, "err", err)
		return false
	}
	return true
}
