package application

import (
	"context"
	"errors"
	"strings"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/notifymail"
)

// MessageCollabView is a message's collaboration state plus its internal notes.
type MessageCollabView struct {
	Collab domain.MessageCollab `json:"collab"`
	Notes  []domain.MessageNote `json:"notes"`
}

// TeamInboxItem pairs collaboration state with its message for the team view.
type TeamInboxItem struct {
	Collab  domain.MessageCollab `json:"collab"`
	Message *domain.Message      `json:"message,omitempty"`
}

func collabActor(ctx context.Context) string {
	if p, ok := PrincipalFrom(ctx); ok && p.LoginID != "" {
		return p.LoginID
	}
	return actorFrom(ctx)
}

func (a *App) getOrDefaultCollab(ctx context.Context, userID, messageID string) (*domain.MessageCollab, error) {
	mc, err := a.Store.GetMessageCollab(ctx, userID, messageID)
	if errors.Is(err, domain.ErrNotFound) {
		return &domain.MessageCollab{MessageID: messageID, UserID: userID, Status: domain.CollabOpen}, nil
	}
	return mc, err
}

// mutateCollab loads (or defaults) the collab state, applies fn, stamps the
// actor, and upserts. It first verifies the message is in the caller's scope.
func (a *App) mutateCollab(ctx context.Context, messageID string, fn func(*domain.MessageCollab)) (*domain.MessageCollab, error) {
	userID := userIDFrom(ctx)
	if _, err := a.Store.GetMessage(ctx, userID, messageID); err != nil {
		return nil, err
	}
	mc, err := a.getOrDefaultCollab(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	fn(mc)
	if mc.Status == "" {
		mc.Status = domain.CollabOpen
	}
	mc.UpdatedBy = collabActor(ctx)
	if err := a.Store.UpsertMessageCollab(ctx, mc); err != nil {
		return nil, err
	}
	return mc, nil
}

// AssignMessage sets (or clears) the assignee of a message.
func (a *App) AssignMessage(ctx context.Context, messageID, assignee string) (*domain.MessageCollab, error) {
	mc, err := a.mutateCollab(ctx, messageID, func(mc *domain.MessageCollab) {
		mc.Assignee = strings.TrimSpace(assignee)
	})
	if err != nil {
		return nil, err
	}
	a.audit(ctx, "collab_assign", "message:"+messageID, "ok", assignee)
	if mc.Assignee != "" {
		// "It is your turn" is worth a mail; clearing an assignment is not.
		subject := "(제목 없음)"
		if m, err := a.Store.GetMessage(ctx, userIDFrom(ctx), messageID); err == nil && strings.TrimSpace(m.Subject) != "" {
			subject = m.Subject
		}
		a.NotifyMail(ctx, notifymail.Assigned(collabActor(ctx), subject, messageID), userIDFrom(ctx), []string{mc.Assignee})
	}
	return mc, nil
}

// SetMessageWorkStatus transitions a message's collaboration status.
func (a *App) SetMessageWorkStatus(ctx context.Context, messageID, status string) (*domain.MessageCollab, error) {
	if _, valid := domain.CanonicalCollabStatus(status); !valid {
		return nil, userErrf("invalid work status (new|needs_action|in_progress|waiting|done; open|pending|resolved are legacy aliases)")
	}
	mc, err := a.mutateCollab(ctx, messageID, func(mc *domain.MessageCollab) { mc.Status = status })
	if err != nil {
		return nil, err
	}
	a.audit(ctx, "collab_status", "message:"+messageID, "ok", status)
	return mc, nil
}

// SetMessageSLA sets the SLA deadline (unix seconds; 0 clears it).
func (a *App) SetMessageSLA(ctx context.Context, messageID string, dueUnix int64) (*domain.MessageCollab, error) {
	if dueUnix < 0 || dueUnix > 253402300799 {
		return nil, userErrf("처리 기한이 올바르지 않습니다")
	}
	result, err := a.mutateCollab(ctx, messageID, func(mc *domain.MessageCollab) {
		// A new deadline is news again; the notifier starts over for it.
		mc.SLADue, mc.SLANotifiedAt = dueUnix, 0
	})
	if err == nil {
		a.audit(ctx, "collab_sla", "message:"+messageID, "ok", "")
	}
	return result, err
}

// GetMessageCollab returns the message's collaboration state (defaulted when
// none exists yet) and its notes.
func (a *App) GetMessageCollab(ctx context.Context, messageID string) (*MessageCollabView, error) {
	userID := userIDFrom(ctx)
	if _, err := a.Store.GetMessage(ctx, userID, messageID); err != nil {
		return nil, err
	}
	mc, err := a.getOrDefaultCollab(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	notes, err := a.Store.ListMessageNotes(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	return &MessageCollabView{Collab: *mc, Notes: nilToEmpty(notes)}, nil
}

// AddMessageNote records an internal (never-sent) team note on a message.
func (a *App) AddMessageNote(ctx context.Context, messageID, body string) (*domain.MessageNote, error) {
	if strings.TrimSpace(body) == "" {
		return nil, userErrf("note body is empty")
	}
	userID := userIDFrom(ctx)
	if _, err := a.Store.GetMessage(ctx, userID, messageID); err != nil {
		return nil, err
	}
	n := &domain.MessageNote{
		ID: persistence.NewID("note"), MessageID: messageID, UserID: userID,
		Author: collabActor(ctx), Body: body,
	}
	if err := a.Store.AddMessageNote(ctx, n); err != nil {
		return nil, err
	}
	a.audit(ctx, "collab_note_add", "message:"+messageID, "ok", "")
	return n, nil
}

// TeamInbox lists messages with collaboration state, optionally filtered by
// status and assignee (§협업 공유 메일함).
func (a *App) TeamInbox(ctx context.Context, status, assignee string, limit int) ([]TeamInboxItem, error) {
	if status != "" {
		if _, valid := domain.CanonicalCollabStatus(status); !valid {
			return nil, userErrf("invalid work status filter")
		}
	}
	userID := userIDFrom(ctx)
	rows, err := a.Store.ListMessageCollab(ctx, userID, status, assignee, limit)
	if err != nil {
		return nil, err
	}
	out := make([]TeamInboxItem, 0, len(rows))
	for i := range rows {
		item := TeamInboxItem{Collab: rows[i]}
		if m, err := a.Store.GetMessage(ctx, userID, rows[i].MessageID); err == nil {
			item.Message = m
		}
		out = append(out, item)
	}
	return out, nil
}
