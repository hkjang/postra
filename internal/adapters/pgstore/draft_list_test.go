package pgstore

import (
	"context"
	"os"
	"postra/internal/domain"
	"testing"
)

func TestPGDraftListOwnerAndCursor(t *testing.T) {
	dsn := os.Getenv("POSTRA_TEST_PG")
	if dsn == "" {
		t.Skip("set POSTRA_TEST_PG to run PostgreSQL draft-list integration")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, other := NewID("draft-owner"), NewID("draft-other")
	for _, user := range []string{owner, other} {
		if err := store.EnsureUser(ctx, user, user); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			d := &domain.Draft{ID: NewID("draft"), UserID: user, AccountID: "list-fixture", Kind: domain.DraftNew, Status: domain.DraftOpen}
			v := &domain.DraftVersion{Subject: user, To: []domain.Address{{Email: "to@corp.local"}}, Author: "user"}
			if err := store.CreateDraft(ctx, d, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, err := store.ListDrafts(ctx, owner, "open", 0, "", 1)
	if err != nil || len(first) != 1 || first[0].Subject != owner || len(first[0].To) != 1 {
		t.Fatalf("first owner page: %+v %v", first, err)
	}
	second, err := store.ListDrafts(ctx, owner, "open", first[0].UpdatedAt, first[0].ID, 5)
	if err != nil || len(second) != 1 || second[0].ID == first[0].ID || second[0].Subject != owner {
		t.Fatalf("second owner page: %+v %v", second, err)
	}
	if err := store.SetDraftStatus(ctx, owner, first[0].ID, domain.DraftDiscarded); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ListDrafts(ctx, owner, "open", 0, "", 10)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("discarded visibility: %+v %v", remaining, err)
	}
}
