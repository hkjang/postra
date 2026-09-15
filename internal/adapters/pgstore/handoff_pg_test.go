package pgstore

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"postra/internal/domain"
)

// TestPGHandoffClaims covers the single-use claim table: consume returns the
// row exactly once even under concurrent collectors, an expired row is
// invisible, and the sweep removes it. Skipped without POSTRA_TEST_PG.
func TestPGHandoffClaims(t *testing.T) {
	dsn := os.Getenv("POSTRA_TEST_PG")
	if dsn == "" {
		t.Skip("set POSTRA_TEST_PG to a pgvector-enabled Postgres DSN to run")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	digest := NewID("digest")
	claim := &domain.HandoffClaim{
		Digest: digest, UserID: "u1", MessageID: "m1", Filename: "a.md",
		ContentType: "text/markdown; charset=utf-8", Bytes: 42, ExpiresAt: now + 300,
	}
	if err := s.InsertHandoffClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertHandoffClaim(ctx, claim); err == nil {
		t.Fatal("the digest is the primary key; a second insert must fail")
	}

	// Many collectors, one winner.
	var wg sync.WaitGroup
	wins := make(chan *domain.HandoffClaim, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := s.ConsumeHandoffClaim(ctx, digest, now); err == nil {
				wins <- got
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("consume: %v", err)
			}
		}()
	}
	wg.Wait()
	close(wins)
	var got []*domain.HandoffClaim
	for c := range wins {
		got = append(got, c)
	}
	if len(got) != 1 {
		t.Fatalf("exactly one collector may win, got %d", len(got))
	}
	if got[0].UserID != "u1" || got[0].MessageID != "m1" || got[0].Filename != "a.md" || got[0].Bytes != 42 || got[0].ExpiresAt != now+300 || got[0].CreatedAt == 0 {
		t.Fatalf("consumed row differs from the inserted one: %+v", got[0])
	}
	if _, err := s.ConsumeHandoffClaim(ctx, digest, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("spent claim: %v, want not found", err)
	}

	// Expired rows are invisible to consume and gone after a sweep.
	expired := &domain.HandoffClaim{Digest: NewID("digest"), UserID: "u1", MessageID: "m1", Filename: "b.md",
		ContentType: "text/markdown; charset=utf-8", Bytes: 1, ExpiresAt: now - 1}
	if err := s.InsertHandoffClaim(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeHandoffClaim(ctx, expired.Digest, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired claim: %v, want not found", err)
	}
	if err := s.SweepHandoffClaims(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeHandoffClaim(ctx, expired.Digest, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("swept claim should be gone even for a clock in the past: %v", err)
	}
}
