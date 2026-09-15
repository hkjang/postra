package application

import (
	"context"
	"encoding/json"
	"errors"
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
	a.runLiveWorker(ctx, "triage-worker", a.triageWorkerInterval, false,
		func() { a.triageOnceMode(ctx, a.triageMode()) })
}

func (a *App) triageMode() string {
	a.runtimeSettings.RLock()
	mode, explicit := a.runtimeSettings.overrides["sync.triage_mode"]
	a.runtimeSettings.RUnlock()
	if !explicit {
		if a.EffectiveConfig().Sync.AutoTriage {
			return "all" // legacy opt-in remains effective until mode is selected
		}
		return "off"
	}
	switch mode {
	case "important_only", "rules", "all":
		return mode
	default:
		return "off"
	}
}

func (a *App) triageWorkerInterval() time.Duration {
	if a.triageMode() == "off" {
		return 0
	}
	if interval := minutesDuration(a.EffectiveConfig().Sync.AutoTriageMinutes); interval > 0 {
		return interval
	}
	return 10 * time.Minute
}

// triageOnce triages one bounded batch per active user. Returns how many
// messages were labelled (exposed for tests).
func (a *App) triageOnce(ctx context.Context) int {
	// Explicit/manual pass retains its previous behavior. The automated worker
	// supplies its effective mode and never invokes this opt-in bypass.
	return a.triageOnceMode(ctx, "all")
}

func (a *App) triageOnceMode(ctx context.Context, mode string) int {
	if mode != "all" && mode != "important_only" && mode != "rules" {
		return 0
	}
	if err := a.checkTaskAIPolicy(ctx, "classify"); err != nil {
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
		ids, err := a.triageCandidates(uctx, since, mode)
		if err != nil || len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return total
			}
			labelled, aiDown := a.triageMessage(uctx, id)
			if aiDown {
				// The provider rejected the call; every remaining message this
				// tick would fail identically. Stop rather than hammer it —
				// the next tick retries from where this left off.
				slog.Warn("triage worker: AI unavailable, ending this pass", "message", id)
				return total
			}
			if labelled {
				total++
			}
		}
	}
	if total > 0 {
		slog.Info("triage worker: classified new mail", "count", total)
	}
	return total
}

func triageMatches(mode string, message *domain.Message, body *domain.MessageBody, rules []domain.MailRule) bool {
	for _, label := range message.Labels {
		if strings.HasPrefix(label, aiLabelPrefix) {
			return false
		}
	}
	switch mode {
	case "all":
		return true
	case "important_only":
		return message.IsImportant
	case "rules":
		for _, rule := range rules {
			if rule.Enabled && ruleMatches(rule, message, body) {
				return true
			}
		}
	}
	return false
}

func (a *App) triageCandidates(ctx context.Context, since int64, mode string) ([]string, error) {
	if mode == "all" {
		return a.Store.MessagesNeedingTriage(ctx, userIDFrom(ctx), since, triageBatch)
	}
	var rules []domain.MailRule
	needBody := false
	if mode == "rules" {
		var err error
		rules, err = a.ListRules(ctx)
		if err != nil {
			return nil, err
		}
		enabled := false
		for _, rule := range rules {
			if !rule.Enabled {
				continue
			}
			enabled = true
			for _, condition := range rule.Conditions {
				needBody = needBody || condition.Field == domain.RuleFieldBody
			}
		}
		if !enabled {
			return nil, nil
		}
	}
	query := domain.SearchQuery{UserID: userIDFrom(ctx), ReceivedSince: since, Limit: 100}
	if mode == "important_only" {
		important := true
		query.IsImportant = &important
	}
	var ids []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := a.Store.Search(ctx, query)
		if err != nil {
			return nil, err
		}
		for i := range page.Messages {
			message := &page.Messages[i]
			var body *domain.MessageBody
			if needBody {
				body, err = a.Store.GetBody(ctx, userIDFrom(ctx), message.ID)
				if err != nil {
					continue // unavailable content cannot satisfy a rule
				}
			}
			if triageMatches(mode, message, body, rules) {
				ids = append(ids, message.ID)
				if len(ids) == triageBatch {
					return ids, nil
				}
			}
		}
		// Page past nonmatching mail; otherwise the first unimportant/unmatched
		// messages would occupy every tick and starve eligible messages forever.
		if page.NextCursor == "" || page.NextCursor == query.Cursor {
			return ids, nil
		}
		query.Cursor = page.NextCursor
	}
}

// triageMessage runs triage for one message and records the outcome as a
// label. It reports whether the message was labelled, and whether the failure
// was the AI provider itself (in which case the caller should stop the pass
// instead of retrying the same fault on every remaining message).
func (a *App) triageMessage(ctx context.Context, messageID string) (labelled, aiUnavailable bool) {
	an, err := a.AnalyzeMessage(ctx, messageID, "triage")
	if errors.Is(err, errAIOutputInvalid) {
		// The model answered, but not in a usable shape. That answer is cached
		// and deterministic, so retrying this message would fail identically
		// every tick while occupying a batch slot. Fall through and label it
		// with the neutral default so it leaves the queue.
		slog.Warn("triage worker: unusable analysis output, defaulting to normal",
			"message", messageID, "err", err)
		an = nil
	} else if err != nil {
		slog.Debug("triage worker: analysis failed", "message", messageID, "err", err)
		return false, true
	}
	var parsed struct {
		Priority      string `json:"priority"`
		ReplyRequired bool   `json:"reply_required"`
	}
	priority, replyRequired := "normal", false
	if an != nil {
		if err := json.Unmarshal([]byte(an.ResultJSON), &parsed); err == nil {
			priority = strings.ToLower(strings.TrimSpace(parsed.Priority))
			replyRequired = parsed.ReplyRequired
		}
	}
	switch priority {
	case "urgent", "high", "normal", "low":
	default:
		priority = "normal" // never write an unvetted label value
	}
	m, err := a.Store.GetMessage(ctx, userIDFrom(ctx), messageID)
	if err != nil {
		return false, false
	}
	label := aiLabelPrefix + priority
	for _, l := range m.Labels {
		if l == label {
			return false, false // already labelled
		}
	}
	m.Labels = append(m.Labels, label)
	if replyRequired {
		m.Labels = append(m.Labels, aiLabelPrefix+"reply-needed")
	}
	// Surface the mail that actually matters through the existing important flag.
	if priority == "urgent" || priority == "high" {
		m.IsImportant = true
	}
	if err := a.Store.UpdateMessage(ctx, m); err != nil {
		slog.Warn("triage worker: label update failed", "message", messageID, "err", err)
		return false, false
	}
	return true, false
}
