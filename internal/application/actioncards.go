package application

import (
	"context"
	"encoding/json"
	"strings"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

// ExtractActionCards runs AI extraction over a message and stores the resulting
// actionable cards in the pending state for user review (§에이전트 액션 실행 카드).
func (a *App) ExtractActionCards(ctx context.Context, messageID string) ([]domain.ActionCard, error) {
	userID := userIDFrom(ctx)
	if _, err := a.Store.GetMessage(ctx, userID, messageID); err != nil {
		return nil, err
	}
	_, input, err := a.messageAsAIInput(ctx, messageID, false)
	if err != nil {
		return nil, err
	}
	an, err := a.runAnalysis(ctx, "action_cards", "message", messageID, "", input)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Cards []struct {
			Type       string  `json:"type"`
			Title      string  `json:"title"`
			Detail     string  `json:"detail"`
			Due        string  `json:"due"`
			Assignee   string  `json:"assignee"`
			Confidence float64 `json:"confidence"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(an.ResultJSON), &parsed); err != nil {
		return nil, userErrf("action card extraction returned unexpected schema: %v", err)
	}
	var out []domain.ActionCard
	for _, c := range parsed.Cards {
		title := strings.TrimSpace(c.Title)
		if title == "" {
			continue
		}
		card := &domain.ActionCard{
			ID: persistence.NewID("act"), UserID: userID, MessageID: messageID,
			Type: normalizeCardType(c.Type), Title: title, Detail: c.Detail,
			Due: c.Due, Assignee: c.Assignee, Status: domain.ActionCardPending,
			Confidence: c.Confidence,
		}
		if err := a.Store.CreateActionCard(ctx, card); err != nil {
			return nil, err
		}
		out = append(out, *card)
	}
	a.audit(ctx, "action_cards_extract", "message:"+messageID, "ok", itoa(len(out)))
	return out, nil
}

func normalizeCardType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "meeting", "todo", "approval", "inquiry":
		return strings.ToLower(t)
	default:
		return "other"
	}
}

func (a *App) ListActionCards(ctx context.Context, status string, limit int) ([]domain.ActionCard, error) {
	return a.Store.ListActionCards(ctx, userIDFrom(ctx), status, limit)
}

func (a *App) GetActionCard(ctx context.Context, id string) (*domain.ActionCard, error) {
	return a.Store.GetActionCard(ctx, userIDFrom(ctx), id)
}

// SetActionCardStatus transitions a card (approve/reject/done). Exporting is a
// separate, explicit step (ExportActionCard).
func (a *App) SetActionCardStatus(ctx context.Context, id, status string) (*domain.ActionCard, error) {
	switch status {
	case domain.ActionCardApproved, domain.ActionCardRejected, domain.ActionCardDone, domain.ActionCardPending:
	default:
		return nil, userErrf("invalid action card status %q", status)
	}
	card, err := a.Store.GetActionCard(ctx, userIDFrom(ctx), id)
	if err != nil {
		return nil, err
	}
	card.Status = status
	if err := a.Store.UpdateActionCard(ctx, card); err != nil {
		return nil, err
	}
	a.audit(ctx, "action_card_status", "card:"+id, "ok", status)
	return card, nil
}

// ActionCardExport is the structured, integration-agnostic payload a caller
// applies to an external system. Postra performs no external write itself.
type ActionCardExport struct {
	Target  string            `json:"target"`
	Card    domain.ActionCard `json:"card"`
	Payload map[string]any    `json:"payload"`
}

// ExportActionCard requires an approved card and returns a normalized payload
// for the chosen target (calendar/jira/itsm/...). The card is marked exported;
// externalRef, when provided by the integration, is recorded for traceability.
func (a *App) ExportActionCard(ctx context.Context, id, target, externalRef string) (*ActionCardExport, error) {
	if strings.TrimSpace(target) == "" {
		return nil, userErrf("export target is required (e.g. calendar, jira, itsm)")
	}
	card, err := a.Store.GetActionCard(ctx, userIDFrom(ctx), id)
	if err != nil {
		return nil, err
	}
	if card.Status != domain.ActionCardApproved && card.Status != domain.ActionCardExported {
		return nil, userErrf("card %s must be approved before export (current: %s)", id, card.Status)
	}
	card.Status = domain.ActionCardExported
	card.ExportTarget = target
	if externalRef != "" {
		card.ExternalRef = externalRef
	}
	if err := a.Store.UpdateActionCard(ctx, card); err != nil {
		return nil, err
	}
	a.audit(ctx, "action_card_export", "card:"+id, "ok", target)
	return &ActionCardExport{
		Target: target,
		Card:   *card,
		Payload: map[string]any{
			"summary":     card.Title,
			"description": card.Detail,
			"type":        card.Type,
			"due":         card.Due,
			"assignee":    card.Assignee,
			"source":      "postra:message:" + card.MessageID,
		},
	}, nil
}
