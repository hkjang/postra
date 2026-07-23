package application

import (
	"context"
	"fmt"
	"strings"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

// embedChunkChars bounds the text embedded per message. MVP uses a single
// chunk (subject + body head); chunking into multiple vectors is a later
// refinement the schema already supports (chunk_id).
const embedChunkChars = 4000

// BuildEmbeddings backfills embeddings for stored messages that lack them,
// so semantic search can rank them. Runs as an async job and respects the
// external-AI policy (mail content leaves the box only if allowed).
func (a *App) BuildEmbeddings(ctx context.Context, accountID string, max int) (*domain.Job, error) {
	if err := a.checkAIPolicy(ctx); err != nil {
		return nil, err
	}
	job := &domain.Job{ID: persistence.NewID("job"), UserID: userIDFrom(ctx), Type: "embed", AccountID: accountID, Status: domain.JobQueued}
	if err := a.Store.CreateJob(ctx, job); err != nil {
		return nil, err
	}
	a.audit(ctx, "embed_start", "account:"+accountID, "ok", "job:"+job.ID)

	jobCtx, cancel := context.WithCancel(a.background)
	if p, ok := PrincipalFrom(ctx); ok {
		jobCtx = WithPrincipal(jobCtx, p)
	}
	a.jobCancels.Store(job.ID, cancel)
	a.workerGroup.Add(1)
	go func() {
		defer a.workerGroup.Done()
		defer a.jobCancels.Delete(job.ID)
		a.runBuildEmbeddings(jobCtx, job, accountID, max)
	}()
	return job, nil
}

func (a *App) runBuildEmbeddings(ctx context.Context, job *domain.Job, accountID string, max int) {
	job.Status = domain.JobRunning
	_ = a.Store.UpdateJob(ctx, job)

	ids, err := a.VectorStore().MessagesMissingEmbeddings(ctx, job.UserID, accountID, max)
	if err != nil {
		job.Status, job.Error = domain.JobFailed, err.Error()
		_ = a.Store.UpdateJob(context.Background(), job)
		return
	}
	var done, failed int64
	for _, id := range ids {
		if ctx.Err() != nil {
			job.Status = domain.JobCancelled
			_ = a.Store.UpdateJob(context.Background(), job)
			return
		}
		if err := a.embedMessage(ctx, accountID, id); err != nil {
			failed++
			continue
		}
		done++
		job.Progress = fmt.Sprintf("%d/%d", done, len(ids))
		_ = a.Store.UpdateJob(ctx, job)
	}
	job.Stats = map[string]int64{"embedded": done, "failed": failed}
	job.Status = domain.JobSucceeded
	if failed > 0 && done == 0 {
		job.Status = domain.JobFailed
	} else if failed > 0 {
		job.Status = domain.JobPartial
	}
	_ = a.Store.UpdateJob(context.Background(), job)
	a.audit(context.Background(), "embed_finish", "account:"+accountID, string(job.Status),
		fmt.Sprintf("embedded=%d failed=%d", done, failed))
}

func (a *App) embedMessage(ctx context.Context, accountID, messageID string) error {
	userID := userIDFrom(ctx)
	m, err := a.Store.GetMessage(ctx, userID, messageID)
	if err != nil {
		return err
	}
	body, _ := a.Store.GetBody(ctx, userID, messageID)
	text := m.Subject
	if body != nil {
		text += "\n" + body.TextBody
	}
	text = truncateRunes(strings.TrimSpace(text), embedChunkChars)
	if text == "" {
		return nil
	}
	res, err := a.AI.Embed(ctx, domain.EmbeddingRequest{Input: []string{text}})
	if err != nil {
		return err
	}
	if len(res.Vectors) == 0 {
		return fmt.Errorf("embedder returned no vector")
	}
	acc := accountID
	if acc == "" {
		acc = m.AccountID
	}
	return a.VectorStore().SaveEmbedding(ctx, userID, acc, messageID, 0, res.Vectors[0], res.Model)
}

// SemanticSearch embeds the query and returns the most similar stored
// messages with their similarity scores and a short explanation (§7 결과 설명).
func (a *App) SemanticSearch(ctx context.Context, query, accountID string, limit int) ([]MessageView, error) {
	if strings.TrimSpace(query) == "" {
		return nil, userErrf("query is empty")
	}
	if err := a.checkAIPolicy(ctx); err != nil {
		return nil, err
	}
	res, err := a.AI.Embed(ctx, domain.EmbeddingRequest{Input: []string{query}})
	if err != nil {
		return nil, err
	}
	if len(res.Vectors) == 0 {
		return nil, userErrf("embedder returned no vector for the query")
	}
	userID := userIDFrom(ctx)
	hits, err := a.VectorStore().SemanticSearch(ctx, userID, accountID, res.Vectors[0], limit)
	if err != nil {
		return nil, err
	}
	out := make([]MessageView, 0, len(hits))
	for _, h := range hits {
		m, err := a.Store.GetMessage(ctx, userID, h.MessageID)
		if err != nil {
			continue // message may have been deleted since indexing
		}
		out = append(out, MessageView{
			Message: *m,
			Score:   h.Score,
			Reason:  fmt.Sprintf("semantic similarity %.3f to query", h.Score),
		})
	}
	a.audit(ctx, "semantic_search", "query", "ok", fmt.Sprintf("hits=%d", len(out)))
	return out, nil
}
