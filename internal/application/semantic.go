package application

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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

type HybridSearchOptions struct {
	Query         string
	AccountID     string
	Limit         int
	RRFKConstant  float64 // default 60.0
	FTSWeight     float64 // default 1.0
	VectorWeight  float64 // default 1.0
	GroupByThread bool
	// Rerank, when true, applies an LLM cross-encoder-style reranking pass over
	// the fused candidates (§검색 파이프라인화). Falls back to RRF order on any
	// AI error or when the AI policy forbids sending content out.
	Rerank bool
}

// HybridSearch combines Full-Text Search (FTS) and Vector Semantic Search using
// Reciprocal Rank Fusion (RRF) (§P1 하이브리드 검색 고도화).
func (a *App) HybridSearch(ctx context.Context, opts HybridSearchOptions) ([]MessageView, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return nil, userErrf("query is empty")
	}
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	if opts.RRFKConstant <= 0 {
		opts.RRFKConstant = 60.0
	}
	if opts.FTSWeight <= 0 {
		opts.FTSWeight = 1.0
	}
	if opts.VectorWeight <= 0 {
		opts.VectorWeight = 1.0
	}

	userID := userIDFrom(ctx)

	// 1. Fetch FTS keyword search results
	ftsResult, ftsErr := a.Store.Search(ctx, domain.SearchQuery{
		UserID:    userID,
		AccountID: opts.AccountID,
		Text:      opts.Query,
		Limit:     opts.Limit * 2,
	})

	// 2. Fetch Semantic Vector search results
	var semViews []MessageView
	var semErr error
	if a.checkAIPolicy(ctx) == nil {
		semViews, semErr = a.SemanticSearch(ctx, opts.Query, opts.AccountID, opts.Limit*2)
	}

	if ftsErr != nil && semErr != nil {
		return nil, fmt.Errorf("hybrid search failed: fts err: %v, sem err: %v", ftsErr, semErr)
	}

	// 3. Compute Reciprocal Rank Fusion (RRF) scores
	scores := make(map[string]float64)
	reasons := make(map[string]string)
	messages := make(map[string]domain.Message)

	if ftsErr == nil && ftsResult != nil {
		for rank, msg := range ftsResult.Messages {
			mID := msg.ID
			messages[mID] = msg
			rrf := opts.FTSWeight / (opts.RRFKConstant + float64(rank+1))
			scores[mID] += rrf
			reasons[mID] = fmt.Sprintf("FTS rank #%d (rrf: %.4f)", rank+1, rrf)
		}
	}

	if semErr == nil {
		for rank, v := range semViews {
			mID := v.Message.ID
			messages[mID] = v.Message
			rrf := opts.VectorWeight / (opts.RRFKConstant + float64(rank+1))
			scores[mID] += rrf

			if existing, ok := reasons[mID]; ok {
				reasons[mID] = fmt.Sprintf("%s + Vector rank #%d (total rrf: %.4f)", existing, rank+1, scores[mID])
			} else {
				reasons[mID] = fmt.Sprintf("Vector rank #%d (rrf: %.4f)", rank+1, rrf)
			}
		}
	}

	// 4. Optional thread aggregation: collapse each thread to its best-scoring
	// message while summing member scores, so a strongly-matching conversation
	// ranks as a unit (§검색 스레드 단위 검색).
	if opts.GroupByThread {
		threadBest := map[string]string{} // threadKey -> representative message ID
		threadScore := map[string]float64{}
		for mID, sc := range scores {
			key := messages[mID].ThreadID
			if key == "" {
				key = mID
			}
			threadScore[key] += sc
			if cur, ok := threadBest[key]; !ok || sc > scores[cur] {
				threadBest[key] = mID
			}
		}
		agg := map[string]float64{}
		for key, mID := range threadBest {
			agg[mID] = threadScore[key]
			reasons[mID] += fmt.Sprintf(" [thread total rrf %.4f]", threadScore[key])
		}
		scores = agg
	}

	// 5. Sort by combined score.
	type rrfHit struct {
		mID   string
		score float64
	}
	var hitList []rrfHit
	for mID, sc := range scores {
		hitList = append(hitList, rrfHit{mID: mID, score: sc})
	}
	sort.Slice(hitList, func(i, j int) bool {
		return hitList[i].score > hitList[j].score
	})

	var out []MessageView
	for _, hit := range hitList {
		out = append(out, MessageView{
			Message: messages[hit.mID],
			Score:   hit.score,
			Reason:  reasons[hit.mID],
		})
	}

	// 6. Optional LLM cross-encoder reranking of the fused candidates.
	reranked := false
	if opts.Rerank && len(out) > 1 && a.checkAIPolicy(ctx) == nil {
		if r := a.rerankViews(ctx, opts.Query, out); r != nil {
			out, reranked = r, true
		}
	}

	// 7. Truncate to the requested page size.
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}

	a.audit(ctx, "hybrid_search", "query:"+opts.Query, "ok",
		fmt.Sprintf("hits=%d reranked=%v", len(out), reranked))
	return out, nil
}

// rerankViews reorders candidates by asking the model to score each one's
// relevance to the query (an LLM stand-in for a cross-encoder). It returns nil
// on any failure so the caller keeps the RRF order. Only the top candidates are
// sent to bound cost.
func (a *App) rerankViews(ctx context.Context, query string, views []MessageView) []MessageView {
	const maxCandidates = 30
	n := len(views)
	if n > maxCandidates {
		n = maxCandidates
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		m := views[i].Message
		fmt.Fprintf(&sb, "[%d] subject=%q from=%s\n", i, m.Subject, m.From.Email)
	}
	res, err := a.AI.Generate(ctx, domain.GenerationRequest{
		System: "You are a search reranker. Score how well each candidate answers the query from 0.0 to 1.0. Respond with JSON only.",
		User: "Query: " + query + "\nCandidates:\n" + sb.String() +
			"\nJSON schema: {\"ranking\":[{\"index\":number,\"score\":number}]}",
		JSONMode: true,
		Task:     "rerank",
	})
	if err != nil {
		return nil
	}
	clean, err := extractJSON(res.Text)
	if err != nil {
		return nil
	}
	var parsed struct {
		Ranking []struct {
			Index int     `json:"index"`
			Score float64 `json:"score"`
		} `json:"ranking"`
	}
	if json.Unmarshal([]byte(clean), &parsed) != nil || len(parsed.Ranking) == 0 {
		return nil
	}
	scoreByIdx := map[int]float64{}
	for _, r := range parsed.Ranking {
		if r.Index >= 0 && r.Index < n {
			scoreByIdx[r.Index] = r.Score
		}
	}
	// Reorder the top-n by model score (stable for unscored), keep the tail.
	head := make([]MessageView, n)
	copy(head, views[:n])
	sort.SliceStable(head, func(i, j int) bool {
		return scoreByIdx[indexOfView(views, head[i])] > scoreByIdx[indexOfView(views, head[j])]
	})
	return append(head, views[n:]...)
}

func indexOfView(views []MessageView, v MessageView) int {
	for i := range views {
		if views[i].Message.ID == v.Message.ID {
			return i
		}
	}
	return -1
}
