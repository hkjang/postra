package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

func leaderTestApp(t *testing.T) (*App, *flakyStore) {
	t.Helper()
	app, _, _, _ := newTestApp(t)
	app.Cfg.WorkerEnabled = true
	store := &flakyStore{Storage: app.Store}
	app.Store = store
	return app, store
}

// The SQLite store serves the process through one connection. A renewal
// queued behind a long write used to wait forever on context.Background,
// freezing the election loop with it.
func TestLeaderRenewalIsBounded(t *testing.T) {
	app, store := leaderTestApp(t)
	old := leaderRenewTimeout
	leaderRenewTimeout = 50 * time.Millisecond
	t.Cleanup(func() { leaderRenewTimeout = old })
	store.leaseHang.Store(true)

	done := make(chan struct{})
	go func() { defer close(done); app.electLeader(context.Background()) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a renewal that cannot reach the database blocked the election loop")
	}
}

// A leader that fails to renew keeps leading while the lease it wrote is still
// valid, and steps down once that lease has lapsed — past it another replica
// may hold the lease, and two leaders would sync and IDLE the same accounts.
func TestLeaderStepsDownOnlyAfterItsLeaseLapses(t *testing.T) {
	app, store := leaderTestApp(t)
	app.electLeader(context.Background())
	if !app.IsLeader() {
		t.Fatal("a free lease was not acquired")
	}
	fault := errors.New("database is locked")
	store.leaseErr.Store(&fault)

	app.electLeader(context.Background())
	if !app.IsLeader() {
		t.Fatal("one failed renewal inside a valid lease must not step down")
	}

	app.leaderMu.Lock()
	app.leaseValidUntil = time.Now().Add(-time.Second)
	app.leaderMu.Unlock()
	app.electLeader(context.Background())
	if app.IsLeader() {
		t.Fatal("a leader kept leading after its lease expired without renewal")
	}

	store.leaseErr.Store(nil)
	app.electLeader(context.Background())
	if !app.IsLeader() {
		t.Fatal("leadership did not return once the database answered again")
	}
}

// Workers wait on the edge rather than sampling IsLeader: each change must
// close the channel handed out before it, and only a change may.
func TestLeaderEdgeSignalsEveryChangeAndOnlyChanges(t *testing.T) {
	app, _ := leaderTestApp(t)
	closed := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	edge := app.leaderEdge()
	app.setLeaderState(false) // already standby: no change
	if closed(edge) {
		t.Fatal("an unchanged state signalled an edge")
	}
	app.setLeaderState(true)
	if !closed(edge) {
		t.Fatal("becoming leader did not signal")
	}
	edge = app.leaderEdge()
	app.setLeaderState(false)
	if !closed(edge) {
		t.Fatal("stepping down did not signal")
	}
}
