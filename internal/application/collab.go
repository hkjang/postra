package application

import (
	"context"
	"errors"
	"strings"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
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
	return mc, nil
}

// SetMessageWorkStatus transitions a message's collaboration status.
func (a *App) SetMessageWorkStatus(ctx context.Context, messageID, status string) (*domain.MessageCollab, error) {
	switch status {
	case domain.CollabOpen, domain.CollabPending, domain.CollabResolved:
	default:
		return nil, userErrf("invalid work status %q (open|pending|resolved)", status)
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
	return a.mutateCollab(ctx, messageID, func(mc *domain.MessageCollab) { mc.SLADue = dueUnix })
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
	return &MessageCollabView{Collab: *mc, Notes: notes}, nil
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
