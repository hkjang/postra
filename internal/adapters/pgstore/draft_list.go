package pgstore

import (
	"context"
	"postra/internal/domain"
)

func (s *Store) ListDrafts(ctx context.Context, userID, status string, beforeUpdated int64, beforeID string, limit int) ([]domain.DraftSummary, error) {
	if limit <= 0 || limit > 201 {
		limit = 201
	}
	rows, err := s.pool.Query(ctx, `SELECT d.id,d.account_id,d.kind,d.status,d.current_version,d.updated_at,COALESCE(v.subject,''),COALESCE(v.to_json,'[]'),v.author
	 FROM drafts d JOIN draft_versions v ON v.draft_id=d.id AND v.version=d.current_version
	 WHERE d.user_id=$1 AND d.status=$2 AND ($3::bigint=0 OR d.updated_at<$3 OR (d.updated_at=$3 AND d.id<$4))
	 ORDER BY d.updated_at DESC,d.id DESC LIMIT $5`, userID, status, beforeUpdated, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.DraftSummary, 0)
	for rows.Next() {
		var d domain.DraftSummary
		var recipients string
		if err := rows.Scan(&d.ID, &d.AccountID, &d.Kind, &d.Status, &d.CurrentVersion, &d.UpdatedAt, &d.Subject, &recipients, &d.Author); err != nil {
			return nil, err
		}
		d.To = addrFromJSON(recipients)
		out = append(out, d)
	}
	return out, rows.Err()
}
