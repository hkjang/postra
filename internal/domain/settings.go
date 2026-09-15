package domain

import (
	"context"
	"strings"
)

// AdminSettingsCompareAndSwapper is an optional storage capability. The public
// settings snapshot (including policy locks) is compared and updates committed
// under a database write lock. Internal user preferences, signatures and worker
// state are excluded, so their unrelated writes cannot invalidate admin forms.
// A false result is a conflict and must not persist any part of updates.
type AdminSettingsCompareAndSwapper interface {
	CompareAndSwapAdminSettings(ctx context.Context, expected, updates map[string]string) (bool, error)
}

func AdminSettingsSnapshot(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		if !strings.HasPrefix(key, "internal.") {
			out[key] = value
		}
	}
	return out
}
