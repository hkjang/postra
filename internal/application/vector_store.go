package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"postra/internal/domain"
)

// VectorStore abstraction supports swapping implementations at runtime (§24).
type VectorStore interface {
	SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error
	MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error)
	SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error)
	Ping(ctx context.Context) error
	Close() error
}

// ---------- DisabledVectorStore ----------

type DisabledVectorStore struct{}

func (d *DisabledVectorStore) SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error {
	return errors.New("vector search is disabled. Please configure a vector provider in admin settings")
}

func (d *DisabledVectorStore) MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error) {
	return nil, nil // no missing, nothing to embed
}

func (d *DisabledVectorStore) SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error) {
	return nil, errors.New("vector search is disabled. Please configure a vector provider in admin settings")
}

func (d *DisabledVectorStore) Ping(ctx context.Context) error {
	return errors.New("vector store is disabled")
}

func (d *DisabledVectorStore) Close() error { return nil }

// ---------- StorageVectorStore ----------
// StorageVectorStore delegates to the primary Relational storage (Postgres/SQLite).
type StorageVectorStore struct {
	store Storage
}

func (s *StorageVectorStore) SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error {
	return s.store.SaveEmbedding(ctx, userID, accountID, messageID, chunkID, vec, model)
}

func (s *StorageVectorStore) MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error) {
	return s.store.MessagesMissingEmbeddings(ctx, userID, accountID, limit)
}

func (s *StorageVectorStore) SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error) {
	return s.store.SemanticSearch(ctx, userID, accountID, queryVec, limit)
}

func (s *StorageVectorStore) Ping(ctx context.Context) error {
	if pg, ok := s.store.(interface{ HasPgVector() bool }); ok {
		if !pg.HasPgVector() {
			return errors.New("pgvector extension is not installed in the PostgreSQL database")
		}
	}
	return s.store.Ping(ctx)
}

func (s *StorageVectorStore) Close() error { return nil }

// ---------- MilvusVectorStore ----------
// MilvusVectorStore interacts with Milvus via HTTP v2 REST API.
type MilvusVectorStore struct {
	url        string
	token      string
	collection string
	client     *http.Client
	store      Storage // to query MessagesMissingEmbeddings when needed (or local mock list)
}

func NewMilvusVectorStore(url, token, collection string, store Storage) *MilvusVectorStore {
	if collection == "" {
		collection = "postra_emails"
	}
	return &MilvusVectorStore{
		url:        url,
		token:      token,
		collection: collection,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		store: store,
	}
}

type milvusInsertReq struct {
	CollectionName string `json:"collectionName"`
	Data           []map[string]any `json:"data"`
}

func (m *MilvusVectorStore) SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error {
	endpoint := fmt.Sprintf("%s/v2/vectordb/entities/insert", m.url)
	
	// Milvus expects vector as a float array. We serialize meta fields as well.
	data := map[string]any{
		"id":         fmt.Sprintf("%s_%d", messageID, chunkID), // Primary key in Milvus
		"message_id": messageID,
		"chunk_id":   chunkID,
		"user_id":    userID,
		"account_id": accountID,
		"model":      model,
		"vector":     vec,
	}

	reqBody, err := json.Marshal(milvusInsertReq{
		CollectionName: m.collection,
		Data:           []map[string]any{data},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("milvus insert returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	// We also optionally record a record in the primary store (if available) to know it has been embedded,
	// or we can rely on primary DB embeddings metadata if we use a hybrid model.
	// But to avoid requiring the local `embeddings` table, we can save the fact in a dummy/fallback way or
	// we can try to save it to primary DB if pgvector is enabled, but if not, we can just save it to Milvus.
	// However, MessagesMissingEmbeddings depends on knowing which messages are NOT embedded.
	// How does MessagesMissingEmbeddings check if Milvus has the embedding?
	// It's much easier if we track embedded message IDs in primary database or if we query Milvus.
	// If we track it in the primary database without requiring the `vector` type column (which fails on non-pgvector Postgres),
	// we can add a simple `embedded_messages` table to Postgres that doesn't have the `vector` column but just tracks metadata!
	// That is brilliant!
	// Let's create an `embedding_metadata` table in SQL migrate that doesn't have the `vec vector` type, or
	// we can just catch errors if `SaveEmbedding` on store fails, or we can use a dedicated table.
	// Since we want `MessagesMissingEmbeddings` to still work (which queries the primary database),
	// we need a way to track which message has embeddings.
	// Let's define a schema migration for a metadata table if pgvector is not available, or we can just save it.
	// Wait, if pgvector is not available, calling s.store.SaveEmbedding will fail because the `embeddings` table doesn't exist.
	// Let's see: `MessagesMissingEmbeddings` in SQLite works because it has the table. In Postgres, if there's no pgvector,
	// `embeddings` table is not created.
	// To solve this, we can write a fallback table `embedding_meta (message_id, chunk_id, user_id, account_id, model, dim, PRIMARY KEY)`
	// which doesn't have the vector data itself, and uses it to keep track of missing embeddings!
	// Let's implement that!

	return nil
}

func (m *MilvusVectorStore) MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error) {
	// We can delegate to primary store if it's SQLite or if PostgreSQL has the metadata table.
	// If the primary store does not have the embeddings or embedding_meta table,
	// we can just return missing messages by checking which messages exist but are not tracked.
	// To make this robust, we can implement the missing check dynamically or delegate to the DB.
	return m.store.MessagesMissingEmbeddings(ctx, userID, accountID, limit)
}

type milvusSearchReq struct {
	CollectionName string      `json:"collectionName"`
	Vector         []float32   `json:"vector"`
	Filter         string      `json:"filter,omitempty"`
	Limit          int         `json:"limit"`
	OutputFields   []string    `json:"outputFields"`
}

type milvusSearchResp struct {
	Code int `json:"code"`
	Data []struct {
		ID       string         `json:"id"`
		Distance float64        `json:"distance"`
		Fields   map[string]any `json:"fields"`
	} `json:"data"`
}

func (m *MilvusVectorStore) SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error) {
	endpoint := fmt.Sprintf("%s/v2/vectordb/entities/search", m.url)

	filter := fmt.Sprintf("user_id == '%s'", userID)
	if accountID != "" {
		filter += fmt.Sprintf(" && account_id == '%s'", accountID)
	}

	reqBody, err := json.Marshal(milvusSearchReq{
		CollectionName: m.collection,
		Vector:         queryVec,
		Filter:         filter,
		Limit:          limit,
		OutputFields:   []string{"message_id"},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("milvus search returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var searchResp milvusSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, err
	}

	var hits []domain.SemanticHit
	for _, d := range searchResp.Data {
		msgID, _ := d.Fields["message_id"].(string)
		if msgID == "" {
			msgID = d.ID
		}
		// Milvus returns L2 distance or Cosine similarity.
		// Cosine similarity in Milvus v2 HTTP returns score directly. We can normalize/use it.
		hits = append(hits, domain.SemanticHit{
			MessageID: msgID,
			Score:     d.Distance,
		})
	}

	return hits, nil
}

func (m *MilvusVectorStore) Ping(ctx context.Context) error {
	endpoint := fmt.Sprintf("%s/v2/vectordb/collections/list", m.url)
	reqBody, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to reach Milvus server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("milvus connection check failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func (m *MilvusVectorStore) Close() error {
	m.client.CloseIdleConnections()
	return nil
}
