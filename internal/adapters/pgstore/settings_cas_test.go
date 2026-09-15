package pgstore

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPGAdminSettingsCASAcrossPools(t *testing.T) {
	first, _ := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	second, err := Open(ctx, os.Getenv("POSTRA_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	prefix := NewID("cas_test")
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
			ok, err := store.CompareAndSwapAdminSettings(ctx, expected, map[string]string{prefix + ".first": value, prefix + ".second": value})
			results <- ok
			errs <- err
		}(index, store)
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	wins := 0
	for ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("two pools must yield one CAS winner, got %d", wins)
	}
	current, _ := first.GetSettings(ctx)
	if current[prefix+".first"] != current[prefix+".second"] {
		t.Fatal("partial transaction")
	}
	if err := second.UpsertSettings(ctx, map[string]string{"internal." + prefix: "private"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := first.CompareAndSwapAdminSettings(ctx, current, map[string]string{prefix + ".unrelated": "safe"}); err != nil || !ok {
		t.Fatalf("private write invalidated admin snapshot: %v", err)
	}
	stale, _ := first.GetSettings(ctx)
	if err := second.UpsertSettings(ctx, map[string]string{prefix + ".legacy": "keep"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := first.CompareAndSwapAdminSettings(ctx, stale, map[string]string{prefix + ".unrelated": "bad"}); err != nil || ok {
		t.Fatalf("legacy writer was not detected: %v", err)
	}
	final, _ := first.GetSettings(ctx)
	if final[prefix+".unrelated"] != "safe" {
		t.Fatal("conflict wrote data")
	}
}
