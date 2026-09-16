package application

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

type CreateActionCardInput struct {
	MessageID string `json:"message_id"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	Due       string `json:"due,omitempty"`
	Assignee  string `json:"assignee,omitempty"`
}

// CreateActionCard creates an explicitly requested, pending, local action. It
// neither spends AI tokens nor registers anything in an external system.
func (a *App) CreateActionCard(ctx context.Context, input CreateActionCardInput) (*domain.ActionCard, error) {
	if p, ok := PrincipalFrom(ctx); ok && p.IsMCPScoped() {
		if err := a.CheckMCPToolPolicy(ctx, "mail_action_card_create"); err != nil {
			return nil, err
		}
	}
	input.Title, input.Assignee, input.Due = strings.TrimSpace(input.Title), strings.TrimSpace(input.Assignee), strings.TrimSpace(input.Due)
	if strings.TrimSpace(input.MessageID) == "" || input.Title == "" || utf8.RuneCountInString(input.Title) > 300 || utf8.RuneCountInString(input.Detail) > 10000 || utf8.RuneCountInString(input.Assignee) > 128 {
		return nil, userErrf("원본 메일과 제목이 필요하며 입력 길이 제한을 지켜야 합니다")
	}
	if input.Type == "" {
		input.Type = "todo"
	}
	switch input.Type {
	case "meeting", "todo", "approval", "inquiry", "other":
	default:
		return nil, userErrf("지원하지 않는 액션 유형입니다")
	}
	if input.Due != "" {
		_, dateErr := time.Parse("2006-01-02", input.Due)
		_, instantErr := time.Parse(time.RFC3339, input.Due)
		if dateErr != nil && instantErr != nil {
			return nil, userErrf("액션 기한은 YYYY-MM-DD 또는 시간대가 포함된 ISO 날짜를 사용하세요")
		}
	}
	userID := userIDFrom(ctx)
	if _, err := a.Store.GetMessage(ctx, userID, input.MessageID); err != nil {
		return nil, err
	}
	card := &domain.ActionCard{ID: persistence.NewID("act"), UserID: userID, MessageID: input.MessageID, Type: input.Type, Title: input.Title, Detail: input.Detail, Due: input.Due, Assignee: input.Assignee, Status: domain.ActionCardPending}
	if err := a.Store.CreateActionCard(ctx, card); err != nil {
		return nil, err
	}
	a.audit(ctx, "action_card_create", "card:"+card.ID, "ok", "")
	return card, nil
}
