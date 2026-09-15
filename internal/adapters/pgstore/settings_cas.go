package pgstore

import (
	"context"
	"maps"
	"slices"

	"postra/internal/domain"
)

var _ domain.AdminSettingsCompareAndSwapper = (*Store)(nil)

// A short table write lock covers missing keys as well as existing rows and
// conflicts with normal INSERT/UPDATE row-exclusive locks. Thus an older or
// direct UpsertSettings writer cannot slip between the comparison and commit.
// Read-committed SELECT after acquiring the lock sees the preceding writer's
// committed values; no external service or secret operation runs in this tx.
func (s *Store) CompareAndSwapAdminSettings(ctx context.Context, expected, updates map[string]string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE system_settings IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, `SELECT key,value FROM system_settings`)
	if err != nil {
		return false, err
	}
	current := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return false, err
		}
		current[key] = value
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if !maps.Equal(domain.AdminSettingsSnapshot(current), domain.AdminSettingsSnapshot(expected)) {
		return false, nil
	}
	for _, key := range slices.Sorted(maps.Keys(updates)) {
		if _, err := tx.Exec(ctx, `INSERT INTO system_settings(key,value,updated_at) VALUES($1,$2,$3) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, updates[key], now()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
