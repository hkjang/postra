package application

import (
	"context"
	"math"
	"testing"

	"postra/internal/domain"
)

// Exercise the application boundary with a non-HTTP provider, so adapter
// validation cannot accidentally be the only protection for the vector index.
type fixedEmbeddingResponse struct {
	domain.AIProvider
	vectors [][]float32
}

func (f fixedEmbeddingResponse) Embed(context.Context, domain.EmbeddingRequest) (domain.EmbeddingResult, error) {
	return domain.EmbeddingResult{Model: "test-embed", Vectors: f.vectors}, nil
}

type embeddingResponseStore struct {
	VectorStore
	batchWrites int
	searches    int
}

func (s *embeddingResponseStore) SaveEmbeddingsBatch(ctx context.Context, userID, accountID string, items []EmbeddingItem) error {
	s.batchWrites++
	return s.VectorStore.SaveEmbeddingsBatch(ctx, userID, accountID, items)
}

func (s *embeddingResponseStore) SemanticSearch(ctx context.Context, userID, accountID string, query []float32, limit int) ([]domain.SemanticHit, error) {
	s.searches++
	return s.VectorStore.SemanticSearch(ctx, userID, accountID, query, limit)
}

func TestEmbeddingBatchRejectsMalformedProviderResponseBeforeWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vectors [][]float32
	}{
		{"missing", nil},
		{"partial", [][]float32{{1, 0}}},
		{"extra", [][]float32{{1, 0}, {0, 1}, {1, 1}}},
		{"empty_first", [][]float32{nil, {1, 0}}},
		{"empty_second", [][]float32{{1, 0}, nil}},
		{"inconsistent_dimension", [][]float32{{1, 0}, {1, 0, 0}}},
		{"nan_second", [][]float32{{1, 0}, {1, float32(math.NaN())}}},
		{"positive_infinity", [][]float32{{1, 0}, {1, float32(math.Inf(1))}}},
		{"negative_infinity", [][]float32{{1, 0}, {1, float32(math.Inf(-1))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			ctx := WithActor(context.Background(), "test")
			account := mustAccount(t, app)
			ids := []string{"embedding-response-a", "embedding-response-b"}
			for _, id := range ids {
				recentMessage(t, app, ctx, account.ID, id, "Embedding response validation", "Message body")
			}
			app.AI = fixedEmbeddingResponse{AIProvider: app.AI, vectors: tc.vectors}
			spy := &embeddingResponseStore{VectorStore: app.VectorStore()}
			app.vectorStore = spy

			if err := app.embedMessagesBatch(ctx, account.ID, ids); err == nil {
				t.Fatal("malformed response was accepted")
			}
			if spy.batchWrites != 0 {
				t.Fatalf("malformed response triggered %d batch writes", spy.batchWrites)
			}
			missing, err := spy.MessagesMissingEmbeddings(ctx, DefaultUserID, account.ID, 10)
			if err != nil || len(missing) != len(ids) {
				t.Fatalf("malformed response changed the index: missing=%v err=%v", missing, err)
			}

			job := &domain.Job{ID: "embedding-response-job", UserID: DefaultUserID, AccountID: account.ID, Type: "embed", Status: domain.JobQueued}
			if err := app.Store.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			app.runBuildEmbeddings(ctx, job, account.ID, 10)
			stored, err := app.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != domain.JobFailed || stored.Stats["embedded"] != 0 || stored.Stats["failed"] != int64(len(ids)) {
				t.Fatalf("malformed response reported job success: %+v", stored)
			}
			if spy.batchWrites != 0 {
				t.Fatalf("failed job triggered %d batch writes", spy.batchWrites)
			}
		})
	}
}

func TestSemanticSearchRejectsMalformedQueryVectorBeforeSearch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vectors [][]float32
	}{
		{"missing", nil},
		{"extra", [][]float32{{1, 0}, {0, 1}}},
		{"empty", [][]float32{nil}},
		{"nan", [][]float32{{1, float32(math.NaN())}}},
		{"positive_infinity", [][]float32{{1, float32(math.Inf(1))}}},
		{"negative_infinity", [][]float32{{1, float32(math.Inf(-1))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			app.AI = fixedEmbeddingResponse{AIProvider: app.AI, vectors: tc.vectors}
			spy := &embeddingResponseStore{VectorStore: app.VectorStore()}
			app.vectorStore = spy
			if _, err := app.SemanticSearch(WithActor(context.Background(), "test"), "find mail", "", 10); err == nil {
				t.Fatal("malformed query vector was accepted")
			}
			if spy.searches != 0 {
				t.Fatalf("malformed query reached the index %d times", spy.searches)
			}
		})
	}
}
