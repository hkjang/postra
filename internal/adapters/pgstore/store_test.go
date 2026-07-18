package pgstore

import (
	"context"
	"os"
	"testing"

	"postra/internal/domain"
)

// TestPGIntegration exercises the PostgreSQL adapter against a real database.
// It runs only when POSTRA_TEST_PG is set to a DSN of a Postgres instance
// with the pgvector extension available, e.g.:
//
//	POSTRA_TEST_PG='postgres://postgres:postgres@localhost:5432/postra_test?sslmode=disable' go test ./internal/adapters/pgstore/
//
// Without it the test is skipped, so CI without Postgres stays green.
func TestPGIntegration(t *testing.T) {
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

	if err := s.EnsureUser(ctx, "u1", "login1"); err != nil {
		t.Fatal(err)
	}
	acc := &domain.MailAccount{ID: NewID("acc"), UserID: "u1", Name: "t", Email: "me@x.com", Status: domain.AccountActive}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	m := &domain.Message{
		ID: NewID("msg"), UserID: "u1", AccountID: acc.ID, UIDL: "uidl1",
		Subject: "budget report", From: domain.Address{Email: "a@x.com"},
		Date: 1000, RawHash: "hash1", RawURI: "local://raw/x",
	}
	body := &domain.MessageBody{MessageID: m.ID, TextBody: "quarterly finance numbers"}
	if err := s.InsertMessage(ctx, m, body, nil); err != nil {
		t.Fatal(err)
	}

	// Full-text search via tsvector.
	res, err := s.Search(ctx, domain.SearchQuery{UserID: "u1", Text: "finance"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("FTS returned %d, want 1", len(res.Messages))
	}

	// pgvector semantic search.
	if err := s.SaveEmbedding(ctx, "u1", acc.ID, m.ID, 0, []float32{1, 0, 0}, "test"); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SemanticSearch(ctx, "u1", "", []float32{0.9, 0.1, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].MessageID != m.ID {
		t.Fatalf("semantic search = %+v, want message %s", hits, m.ID)
	}
	if hits[0].Score < 0.9 {
		t.Fatalf("cosine score %.3f too low for aligned vectors", hits[0].Score)
	}

	// Dedup + delete.
	if dup, _ := s.IsDuplicateHash(ctx, acc.ID, "hash1"); !dup {
		t.Fatal("hash1 should be a duplicate")
	}
	if _, err := s.DeleteMessage(ctx, "u1", m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMessage(ctx, "u1", m.ID); err == nil {
		t.Fatal("message should be gone after delete")
	}
}
