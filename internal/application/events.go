package application

import (
	"context"
	"sort"
	"strconv"

	"postra/internal/domain"
)

// NotificationEvent is intentionally a whitelist, never a serialization of a
// Job, OutboundMessage, ActionCard, or AuditEvent. Their descriptions, errors,
// recipients, progress strings, provider responses and secrets stay private.
type NotificationEvent struct {
	ID         string `json:"id"`
	Category   string `json:"category"`
	Kind       string `json:"kind"`
	ResourceID string `json:"resource_id,omitempty"`
	Status     string `json:"status"`
	At         int64  `json:"at"`
}

type NotificationSnapshot struct {
	UserID              string              `json:"user_id"`
	Enabled             bool                `json:"enabled"`
	PollSeconds         int                 `json:"poll_seconds"`
	PreferencesRevision string              `json:"preferences_revision"`
	Events              []NotificationEvent `json:"events"`
}

func (a *App) NotificationPollSeconds() int {
	seconds := a.SettingInt("notifications.poll_seconds")
	if seconds < 5 {
		return 5
	}
	if seconds > 300 {
		return 300
	}
	return seconds
}

// NotificationEvents is the shared snapshot used by REST SSE, its JSON polling
// fallback, and MCP. Authentication is revalidated by streaming transports.
func (a *App) NotificationEvents(ctx context.Context) (*NotificationSnapshot, error) {
	principal, ok := PrincipalFrom(ctx)
	if !ok || principal.UserID == "" {
		return nil, &domain.PublicError{Code: "authentication_required", Message: "로그인이 필요합니다.", Status: 401}
	}
	if principal.AuthMethod == "mcp_key" {
		if err := a.CheckMCPToolPolicy(ctx, "mail_events"); err != nil {
			return nil, err
		}
	}
	out := &NotificationSnapshot{UserID: principal.UserID, Enabled: a.SettingBool("notifications.enabled"), PollSeconds: a.NotificationPollSeconds(), Events: []NotificationEvent{}}
	preferences, err := a.PersonalSettings(ctx, "")
	if err != nil {
		return nil, err
	}
	// The hash detects policy/theme changes without disclosing setting values,
	// and remains available when activity notifications themselves are disabled.
	out.PreferencesRevision = preferences.Revision
	if !out.Enabled {
		return out, nil
	}
	prefs := map[string]string{}
	for _, field := range preferences.Fields {
		prefs[field.Key] = field.Value
	}
	allowed := func(category string) bool { return prefs["notifications."+category] == "true" }
	if allowed("sync") || allowed("ai") {
		jobs, err := a.ListJobs(ctx, 50)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			category := "ai"
			switch job.Type {
			case "sync":
				category = "sync"
			case "analysis", "embed", "embedding", "embeddings", "embedding_build", "triage":
			default:
				continue
			}
			if job.UserID != principal.UserID || !allowed(category) {
				continue
			}
			out.Events = append(out.Events, NotificationEvent{ID: "job:" + job.ID, ResourceID: job.ID, Category: category, Kind: "job", Status: notificationStatus(string(job.Status)), At: job.UpdatedAt})
		}
	}
	if allowed("send") {
		// No enrichment with draft subjects or recipients is needed here.
		messages, err := a.Store.ListOutbound(ctx, principal.UserID, 50)
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			if message.UserID == principal.UserID {
				out.Events = append(out.Events, NotificationEvent{ID: "outbound:" + message.ID, ResourceID: message.ID, Category: "send", Kind: "outbound", Status: notificationStatus(string(message.Status)), At: message.UpdatedAt})
			}
		}
	}
	if allowed("action") {
		cards, err := a.ListActionCards(ctx, "", 50)
		if err != nil {
			return nil, err
		}
		for _, card := range cards {
			if card.UserID == principal.UserID {
				out.Events = append(out.Events, NotificationEvent{ID: "action:" + card.ID, ResourceID: card.ID, Category: "action", Kind: "action", Status: notificationStatus(card.Status), At: card.UpdatedAt})
			}
		}
	}
	if allowed("ai") || allowed("security") || allowed("action") {
		audits, err := a.SearchAudit(ctx, 100)
		if err != nil {
			return nil, err
		}
		for _, audit := range audits {
			category := ""
			switch audit.Action {
			case "ai_analysis":
				category = "ai"
			case "login", "password_reset", "mcp_key_create", "mcp_key_revoke", "mcp_key_admin_revoke", "mcp_key_scopes_update", "ai_pii_masked", "attachment_blocked":
				category = "security"
			case "collab_assign", "collab_status", "collab_note_add", "collab_sla":
				category = "action"
			}
			if audit.UserID != principal.UserID || category == "" || !allowed(category) {
				continue
			}
			out.Events = append(out.Events, NotificationEvent{ID: "audit:" + strconv.FormatInt(audit.ID, 10), Category: category, Kind: "activity", Status: notificationStatus(audit.Result), At: audit.At})
		}
	}
	sort.Slice(out.Events, func(i, j int) bool {
		if out.Events[i].At == out.Events[j].At {
			return out.Events[i].ID < out.Events[j].ID
		}
		return out.Events[i].At > out.Events[j].At
	})
	return out, nil
}

func notificationStatus(status string) string {
	switch status {
	case "queued", "running", "succeeded", "partially_succeeded", "failed", "cancelled", "uncertain", "send_uncertain", "pending", "sending", "sent", "retry_pending", "retry_wait", "approved", "rejected", "done", "exported", "ok", "denied", "error":
		return status
	default:
		return "unknown"
	}
}
