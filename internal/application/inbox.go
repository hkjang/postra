package application

import (
	"context"
	"time"

	"postra/internal/domain"
)

// WorkInbox is a task-oriented view of the mailbox that groups messages into
// work buckets instead of a flat chronological list (§UX AI 업무 받은편지함). It is
// signal-based (importance, snooze, attachments, labels) so it is cheap and
// deterministic; AI classification/rules can enrich the same buckets over time.
type WorkInbox struct {
	Account     string           `json:"account,omitempty"`
	Important   []domain.Message `json:"important"`
	SnoozedDue  []domain.Message `json:"snoozed_due"`
	Attention   []domain.Message `json:"attention"`
	Reference   []domain.Message `json:"reference"`
	Counts      map[string]int   `json:"counts"`
	GeneratedAt int64            `json:"generated_at"`
}

// WorkInbox builds the triage view from the active inbox window (archived and
// still-snoozed messages are excluded by the "inbox" folder filter).
func (a *App) WorkInbox(ctx context.Context, accountID string, limit int) (*WorkInbox, error) {
	userID := userIDFrom(ctx)
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	res, err := a.Store.Search(ctx, domain.SearchQuery{
		UserID: userID, AccountID: accountID, Folder: "inbox", Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	inbox := &WorkInbox{Account: accountID, Counts: map[string]int{}, GeneratedAt: now}
	for _, m := range res.Messages {
		switch {
		case m.SnoozedUntil > 0 && m.SnoozedUntil <= now:
			inbox.SnoozedDue = append(inbox.SnoozedDue, m)
		case m.IsImportant:
			inbox.Important = append(inbox.Important, m)
		case m.HasAttachments:
			inbox.Attention = append(inbox.Attention, m)
		default:
			inbox.Reference = append(inbox.Reference, m)
		}
	}
	inbox.Counts["important"] = len(inbox.Important)
	inbox.Counts["snoozed_due"] = len(inbox.SnoozedDue)
	inbox.Counts["attention"] = len(inbox.Attention)
	inbox.Counts["reference"] = len(inbox.Reference)
	a.audit(ctx, "work_inbox", "account:"+accountID, "ok", "")
	return inbox, nil
}
