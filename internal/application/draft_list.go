package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"postra/internal/domain"
)

type DraftList struct {
	Drafts     []domain.DraftSummary `json:"drafts"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

type draftCursor struct {
	Updated int64  `json:"updated"`
	ID      string `json:"id"`
}

func (a *App) ListDrafts(ctx context.Context, status string, limit int, cursor string) (*DraftList, error) {
	if err := CheckMCPScopes(ctx, "mail.draft"); err != nil {
		return nil, err
	}
	if status == "" {
		status = string(domain.DraftOpen)
	}
	switch domain.DraftStatus(status) {
	case domain.DraftOpen, domain.DraftApproved, domain.DraftSent, domain.DraftDiscarded:
	default:
		return nil, userErrf("invalid draft status")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var before draftCursor
	if cursor != "" {
		if len(cursor) > 512 {
			return nil, userErrf("invalid draft cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(data, &before) != nil || before.Updated <= 0 || before.ID == "" {
			return nil, userErrf("invalid draft cursor")
		}
	}
	rows, err := a.Store.ListDrafts(ctx, userIDFrom(ctx), status, before.Updated, before.ID, limit+1)
	if err != nil {
		return nil, err
	}
	result := &DraftList{Drafts: nilToEmpty(rows)}
	if len(rows) > limit {
		result.Drafts = rows[:limit]
		last := rows[limit-1]
		encoded, _ := json.Marshal(draftCursor{Updated: last.UpdatedAt, ID: last.ID})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return result, nil
}
