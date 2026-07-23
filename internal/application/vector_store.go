package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"postra/internal/domain"
)

type EmbeddingItem = domain.EmbeddingItem

// VectorStore abstraction supports swapping implementations at runtime (§24).
type VectorStore interface {
	SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error
	SaveEmbeddingsBatch(ctx context.Context, userID, accountID string, items []EmbeddingItem) error
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

func (d *DisabledVectorStore) SaveEmbeddingsBatch(ctx context.Context, userID, accountID string, items []EmbeddingItem) error {
	return errors.New("vector search is disabled. Please configure a vector provider in admin settings")
}

func (d *DisabledVectorStore) MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error) {
	return nil, nil
}

func (d *DisabledVectorStore) SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error) {
	return nil, errors.New("vector search is disabled. Please configure a vector provider in admin settings")
}

func (d *DisabledVectorStore) Ping(ctx context.Context) error {
	return errors.New("vector store is disabled")
}

func (d *DisabledVectorStore) Close() error { return nil }

// ---------- StorageVectorStore ----------
type StorageVectorStore struct {
	store Storage
}

func (s *StorageVectorStore) SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error {
	return s.store.SaveEmbedding(ctx, userID, accountID, messageID, chunkID, vec, model)
}

func (s *StorageVectorStore) SaveEmbeddingsBatch(ctx context.Context, userID, accountID string, items []EmbeddingItem) error {
	for _, item := range items {
		err := s.store.SaveEmbedding(ctx, userID, accountID, item.MessageID, item.ChunkID, item.Vector, item.Model)
		if err != nil {
			return err
		}
	}
	return nil
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
type MilvusVectorStore struct {
	url        string
	token      string
	collection string
	client     *http.Client
	store      Storage
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
			Timeout: 15 * time.Second,
		},
		store: store,
	}
}

type milvusInsertReq struct {
	CollectionName string           `json:"collectionName"`
	Data           []map[string]any `json:"data"`
}

// milvusStatusResp captures the logical status the Milvus v2 REST API returns
// in the body. code==0 is success; a non-zero code is a failure even when the
// HTTP status is 200, so it must be checked explicitly.
type milvusStatusResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// milvusOKCode reports whether a Milvus REST status code means success. The v2
// RESTful API returns 0 on success; some versions/proxies use 200.
func milvusOKCode(code int) bool { return code == 0 || code == 200 }

// checkMilvusStatus reads the response body and returns an error when Milvus
// reports a logical failure (non-success code) despite HTTP 200.
func checkMilvusStatus(op string, body []byte) error {
	var st milvusStatusResp
	if err := json.Unmarshal(body, &st); err != nil {
		// A body we cannot parse as a status envelope is itself suspicious.
		return fmt.Errorf("milvus %s: unparseable response: %s", op, truncateStr(string(body), 200))
	}
	if !milvusOKCode(st.Code) {
		msg := st.Message
		if msg == "" {
			msg = "no message"
		}
		return fmt.Errorf("milvus %s failed (code %d): %s", op, st.Code, msg)
	}
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (m *MilvusVectorStore) SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error {
	return m.SaveEmbeddingsBatch(ctx, userID, accountID, []EmbeddingItem{
		{MessageID: messageID, ChunkID: chunkID, Vector: vec, Model: model},
	})
}

func (m *MilvusVectorStore) SaveEmbeddingsBatch(ctx context.Context, userID, accountID string, items []EmbeddingItem) error {
	if len(items) == 0 {
		return nil
	}
	endpoint := fmt.Sprintf("%s/v2/vectordb/entities/insert", m.url)

	var data []map[string]any
	for _, item := range items {
		data = append(data, map[string]any{
			"id":         fmt.Sprintf("%s_%d", item.MessageID, item.ChunkID),
			"message_id": item.MessageID,
			"chunk_id":   item.ChunkID,
			"user_id":    userID,
			"account_id": accountID,
			"model":      item.Model,
			"vector":     item.Vector,
		})
	}

	reqBody, err := json.Marshal(milvusInsertReq{
		CollectionName: m.collection,
		Data:           data,
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

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("milvus insert returned HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 300))
	}
	// HTTP 200 is not proof of success — Milvus reports logical failures in the
	// body's code field (P1 Milvus 응답 검증).
	if err := checkMilvusStatus("insert", body); err != nil {
		return err
	}

	// Save embedding meta to primary DB to mark as embedded using batch transaction.
	if err := m.store.SaveEmbeddingsBatch(ctx, userID, accountID, items); err != nil {
		return fmt.Errorf("failed to save embedding metadata to primary database: %w", err)
	}

	return nil
}

func (m *MilvusVectorStore) MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error) {
	return m.store.MessagesMissingEmbeddings(ctx, userID, accountID, limit)
}

type milvusSearchReq struct {
	CollectionName string    `json:"collectionName"`
	Vector         []float32 `json:"vector"`
	Filter         string    `json:"filter,omitempty"`
	Limit          int       `json:"limit"`
	OutputFields   []string  `json:"outputFields"`
	MetricType     string    `json:"metricType,omitempty"`
}

type milvusSearchResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    []struct {
		ID       string         `json:"id"`
		Distance float64        `json:"distance"`
		Fields   map[string]any `json:"fields"`
	} `json:"data"`
}

func (m *MilvusVectorStore) SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error) {
	endpoint := fmt.Sprintf("%s/v2/vectordb/entities/search", m.url)

	filter := fmt.Sprintf("user_id == '%s'", escapeMilvusString(userID))
	if accountID != "" {
		filter += fmt.Sprintf(" && account_id == '%s'", escapeMilvusString(accountID))
	}

	reqBody, err := json.Marshal(milvusSearchReq{
		CollectionName: m.collection,
		Vector:         queryVec,
		Filter:         filter,
		Limit:          limit,
		OutputFields:   []string{"message_id"},
		MetricType:     "COSINE",
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

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("milvus search returned HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 300))
	}

	var searchResp milvusSearchResp
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return nil, err
	}
	// Guard against HTTP 200 with a logical failure code.
	if !milvusOKCode(searchResp.Code) {
		msg := searchResp.Message
		if msg == "" {
			msg = "no message"
		}
		return nil, fmt.Errorf("milvus search failed (code %d): %s", searchResp.Code, msg)
	}

	var hits []domain.SemanticHit
	for _, d := range searchResp.Data {
		msgID, _ := d.Fields["message_id"].(string)
		if msgID == "" {
			msgID = d.ID
		}
		hits = append(hits, domain.SemanticHit{
			MessageID: msgID,
			Score:     d.Distance,
		})
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })

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

func escapeMilvusString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	return s
}
