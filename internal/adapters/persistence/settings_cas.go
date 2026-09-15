package persistence

import (
	"context"
	"maps"
	"slices"

	"postra/internal/domain"
)

var _ domain.AdminSettingsCompareAndSwapper = (*Store)(nil)

// Acquire the SQLite writer reservation before the snapshot SELECT. A deferred
// read transaction upgraded after another writer commits can yield BUSY_SNAPSHOT
// without honoring busy_timeout. A no-row UPDATE starts a write transaction
// without changing a setting, and serializes even ordinary UpsertSettings calls
// from another Store/process. sql.Tx also reliably rolls back on cancellation.
func (s *Store) CompareAndSwapAdminSettings(ctx context.Context, expected, updates map[string]string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE system_settings SET value=value WHERE 0`); err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM system_settings`)
	if err != nil {
		return false, err
	}
	current := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close() // retain the original scan error
			return false, err
		}
		current[key] = value
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	if !maps.Equal(domain.AdminSettingsSnapshot(current), domain.AdminSettingsSnapshot(expected)) {
		return false, nil
	}
	keys := slices.Sorted(maps.Keys(updates))
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO system_settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, updates[key], now()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
