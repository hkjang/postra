package persistence

import (
	"path/filepath"
	"strings"
	"testing"
)

// The background workers sweep by owner and arrival time on a fixed cadence —
// triage every few minutes, for the life of the deployment. Without a matching
// index that is a full table scan whose cost grows with the mailbox, so assert
// the planner actually has one to use.
func TestPeriodicSweepQueryUsesIndex(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rows, err := store.db.Query(
		`EXPLAIN QUERY PLAN SELECT id FROM messages
		 WHERE user_id=? AND created_at >= ? ORDER BY date DESC LIMIT 20`, "u", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	got := plan.String()
	if !strings.Contains(got, "idx_messages_user_created") {
		t.Fatalf("periodic sweep is not using the (user_id, created_at) index:\n%s", got)
	}
}
