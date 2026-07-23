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
	
	const batchSize = 20
	var done, failed int64

	for i := 0; i < len(ids); i += batchSize {
		if ctx.Err() != nil {
			job.Status = domain.JobCancelled
			_ = a.Store.UpdateJob(context.Background(), job)
			return
		}
		
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batchIds := ids[i:end]
		
		err := a.embedMessagesBatch(ctx, accountID, batchIds)
		if err != nil {
			failed += int64(len(batchIds))
			a.audit(ctx, "embed_batch_failed", "account:"+accountID, "error", err.Error())
		} else {
			done += int64(len(batchIds))
		}

		job.Progress = fmt.Sprintf("%d/%d", done+failed, len(ids))
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

func (a *App) embedMessagesBatch(ctx context.Context, accountID string, messageIDs []string) error {
	userID := userIDFrom(ctx)
	
	var texts []string
	var validIDs []string
	var accs []string

	for _, mID := range messageIDs {
		m, err := a.Store.GetMessage(ctx, userID, mID)
		if err != nil {
			continue
		}
		body, _ := a.Store.GetBody(ctx, userID, mID)
		text := m.Subject
		if body != nil {
			text += "\n" + body.TextBody
		}
		text = truncateRunes(strings.TrimSpace(text), embedChunkChars)
		if text == "" {
			// Mark as embedded with dummy none model to avoid re-scanning empty emails
			_ = a.VectorStore().SaveEmbedding(ctx, userID, m.AccountID, mID, 0, nil, "none")
			continue
		}
		texts = append(texts, text)
		validIDs = append(validIDs, mID)
		acc := accountID
		if acc == "" {
			acc = m.AccountID
		}
		accs = append(accs, acc)
	}

	if len(texts) == 0 {
		return nil
	}

	res, err := a.AI.Embed(ctx, domain.EmbeddingRequest{Input: texts})
	if err != nil {
		return err
	}
	if len(res.Vectors) == 0 {
		return fmt.Errorf("embedder returned no vectors")
	}

	var items []EmbeddingItem
	for idx, mID := range validIDs {
		if idx >= len(res.Vectors) {
			break
		}
		items = append(items, EmbeddingItem{
			MessageID: mID,
			ChunkID:   0,
			Vector:    res.Vectors[idx],
			Model:     res.Model,
		})
	}

	targetAcc := accountID
	if targetAcc == "" && len(accs) > 0 {
		targetAcc = accs[0]
	}

	return a.VectorStore().SaveEmbeddingsBatch(ctx, userID, targetAcc, items)
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
