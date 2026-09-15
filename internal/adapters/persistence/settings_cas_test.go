package persistence

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAdminSettingsCASAcrossSQLiteStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for attempt := 0; attempt < 8; attempt++ {
		expected, err := first.GetSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan bool, 2)
		errs := make(chan error, 2)
		var wait sync.WaitGroup
		for index, store := range []*Store{first, second} {
			wait.Add(1)
			go func(index int, store *Store) {
				defer wait.Done()
				<-start
				value := []string{"A", "B"}[index]
				committed, err := store.CompareAndSwapAdminSettings(ctx, expected, map[string]string{"cas.first": value, "cas.second": value, "cas.round": strconv.Itoa(attempt)})
				results <- committed
				errs <- err
			}(index, store)
		}
		close(start)
		wait.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("SQLite contention must not become an unsafe write: %v", err)
			}
		}
		wins := 0
		for committed := range results {
			if committed {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("expected one winner, got %d", wins)
		}
		current, _ := first.GetSettings(ctx)
		if current["cas.first"] != current["cas.second"] {
			t.Fatal("partial transaction observed")
		}
	}
	expected, _ := first.GetSettings(ctx)
	if err := second.UpsertSettings(ctx, map[string]string{"internal.preferences.other": "private"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := first.CompareAndSwapAdminSettings(ctx, expected, map[string]string{"cas.safe": "new"}); err != nil || !ok {
		t.Fatalf("unrelated private writes invalidated admin snapshot: %v", err)
	}
	stale, _ := first.GetSettings(ctx)
	if err := second.UpsertSettings(ctx, map[string]string{"cas.added-by-legacy": "preserve"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := first.CompareAndSwapAdminSettings(ctx, stale, map[string]string{"cas.safe": "overwritten"}); err != nil || ok {
		t.Fatalf("ordinary writer/new public key was not detected: %v", err)
	}
	final, _ := first.GetSettings(ctx)
	if final["cas.safe"] != "new" || final["internal.preferences.other"] != "private" {
		t.Fatal("conflict changed existing settings")
	}
}

func TestAdminSettingsCASCanceledTransactionReleasesSQLiteWriter(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := store.CompareAndSwapAdminSettings(ctx, nil, map[string]string{"setting": "bad"}); err == nil || ok {
		t.Fatal("canceled write succeeded")
	}
	if ok, err := store.CompareAndSwapAdminSettings(context.Background(), nil, map[string]string{"setting": "good"}); err != nil || !ok {
		t.Fatalf("writer remained locked: %v", err)
	}
}

func TestAdminSettingsCASBusyFailureDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := second.db.ExecContext(ctx, `PRAGMA busy_timeout=50`); err != nil {
		t.Fatal(err)
	}
	tx, err := first.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE system_settings SET value=value WHERE 0`); err != nil {
		t.Fatal(err)
	}
	if ok, err := second.CompareAndSwapAdminSettings(ctx, nil, map[string]string{"cas.busy": "bad"}); err == nil || ok {
		t.Fatal("busy writer unexpectedly reported a commit")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if ok, err := second.CompareAndSwapAdminSettings(ctx, nil, map[string]string{"cas.busy": "good"}); err != nil || !ok {
		t.Fatalf("busy failure wrote data or left a lock: %v", err)
	}
}
