package persistence

import (
	"context"
	"encoding/binary"
	"math"
	"sort"

	"postra/internal/domain"
)

// float32sToBytes / bytesToFloat32s serialize vectors for the BLOB column.
func float32sToBytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func bytesToFloat32s(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

func (s *Store) SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO embeddings
	 (message_id,chunk_id,user_id,account_id,model,dim,vec) VALUES (?,?,?,?,?,?,?)
	 ON CONFLICT(message_id,chunk_id) DO UPDATE SET model=excluded.model, dim=excluded.dim, vec=excluded.vec`,
		messageID, chunkID, userID, accountID, model, len(vec), float32sToBytes(vec))
	return err
}

// MessagesMissingEmbeddings returns IDs of stored messages that have no
// embedding yet, so BuildEmbeddings can backfill incrementally.
func (s *Store) MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	args := []any{userID}
	q := `SELECT m.id FROM messages m
	 WHERE m.user_id=? AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.message_id=m.id)`
	if accountID != "" {
		q += ` AND m.account_id=?`
		args = append(args, accountID)
	}
	q += ` ORDER BY m.date DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SemanticSearch ranks stored embeddings by cosine similarity to queryVec.
// SQLite has no native vector index, so this scans the (user, account)-scoped
// vectors and ranks in Go — fine for personal/embedded scale. The PostgreSQL
// adapter overrides this with a pgvector index for server scale.
func (s *Store) SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error) {
	if limit <= 0 {
		limit = 10
	}
	args := []any{userID}
	q := `SELECT message_id, vec FROM embeddings WHERE user_id=?`
	if accountID != "" {
		q += ` AND account_id=?`
		args = append(args, accountID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	qNorm := norm(queryVec)
	best := map[string]float64{} // best score per message across its chunks
	for rows.Next() {
		var mid string
		var blob []byte
		if err := rows.Scan(&mid, &blob); err != nil {
			return nil, err
		}
		score := cosine(queryVec, qNorm, bytesToFloat32s(blob))
		if cur, ok := best[mid]; !ok || score > cur {
			best[mid] = score
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hits := make([]domain.SemanticHit, 0, len(best))
	for mid, sc := range best {
		hits = append(hits, domain.SemanticHit{MessageID: mid, Score: sc})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func norm(v []float32) float64 {
	var s float64
	for _, f := range v {
		s += float64(f) * float64(f)
	}
	return math.Sqrt(s)
}

func cosine(a []float32, aNorm float64, b []float32) float64 {
	if len(a) != len(b) || aNorm == 0 {
		return 0
	}
	var dot, bNorm float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		bNorm += float64(b[i]) * float64(b[i])
	}
	bNorm = math.Sqrt(bNorm)
	if bNorm == 0 {
		return 0
	}
	return dot / (aNorm * bNorm)
}
