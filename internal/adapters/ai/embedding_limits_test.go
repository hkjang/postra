package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"postra/internal/domain"
)

func TestEmbedValidatesAndOrdersResponses(t *testing.T) {
	tests := []struct {
		name, response string
		valid          bool
	}{
		{"reordered", `{"data":[{"index":1,"embedding":[2,3]},{"index":0,"embedding":[0,1]}]}`, true},
		{"legacy_without_indices", `{"data":[{"embedding":[0,1]},{"embedding":[2,3]}]}`, true},
		{"partial", `{"data":[{"index":0,"embedding":[0,1]}]}`, false},
		{"extra", `{"data":[{"index":0,"embedding":[0,1]},{"index":1,"embedding":[2,3]},{"index":2,"embedding":[4,5]}]}`, false},
		{"duplicate", `{"data":[{"index":0,"embedding":[0,1]},{"index":0,"embedding":[2,3]}]}`, false},
		{"out_of_range", `{"data":[{"index":0,"embedding":[0,1]},{"index":2,"embedding":[2,3]}]}`, false},
		{"negative", `{"data":[{"index":-1,"embedding":[0,1]},{"index":1,"embedding":[2,3]}]}`, false},
		{"mixed_index", `{"data":[{"index":0,"embedding":[0,1]},{"embedding":[2,3]}]}`, false},
		{"empty_vector", `{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[2,3]}]}`, false},
		{"dimensions", `{"data":[{"index":0,"embedding":[0]},{"index":1,"embedding":[2,3]}]}`, false},
		{"overflow", `{"data":[{"index":0,"embedding":[1e100,1]},{"index":1,"embedding":[2,3]}]}`, false},
		{"nonfinite", `{"data":[{"index":0,"embedding":[NaN,1]},{"index":1,"embedding":[2,3]}]}`, false},
		{"empty_response", `{"data":[]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()
			result, err := New(limitsConfig(server.URL), nil).Embed(context.Background(), domain.EmbeddingRequest{Input: []string{"a", "b"}})
			if tt.valid {
				if err != nil || !reflect.DeepEqual(result.Vectors, [][]float32{{0, 1}, {2, 3}}) {
					t.Fatalf("wrong vectors %+v %v", result, err)
				}
			} else {
				var public *domain.PublicError
				if !errors.As(err, &public) || public.Code != "invalid_embedding" || len(result.Vectors) != 0 {
					t.Fatalf("invalid embedding accepted %+v %v", result, err)
				}
			}
		})
	}
}

func TestEmbedUsesItsOwnModelAndBudgetsEachInput(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path != "/embed/models" {
				t.Errorf("wrong discovery endpoint %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"chosen","max_model_len":65536},{"id":"embed","max_model_len":32}]}`))
			return
		}
		posts.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0,1]},{"index":1,"embedding":[2,3]}]}`))
	}))
	defer server.Close()
	cfg := limitsConfig(server.URL + "/chat")
	cfg.EmbedBaseURL, cfg.EmbedModel = server.URL+"/embed", "embed"
	p := New(cfg, nil)
	// The combined batch exceeds the model limit, but each input fits.
	_, err := p.Embed(context.Background(), domain.EmbeddingRequest{Input: []string{strings.Repeat("한", 10), strings.Repeat("글", 10)}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Embed(context.Background(), domain.EmbeddingRequest{Input: []string{strings.Repeat("한", 11)}})
	var public *domain.PublicError
	if !errors.As(err, &public) || public.Code != "context_limit" || posts.Load() != 1 {
		t.Fatalf("embedding cap ignored %v calls=%d", err, posts.Load())
	}
}

func TestEmbeddingMetadataFallbackDoesNotClaimChatLimit(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for _, auto := range []bool{true, false} {
		cfg := limitsConfig(server.URL)
		cfg.AutoContextLength = auto
		limits, err := New(cfg, nil).ModelLimits(context.Background(), "", true)
		if err != nil || limits.ContextLength != 0 || limits.MaxOutputTokens != 0 {
			t.Fatalf("chat limit mislabeled as embedding %+v %v", limits, err)
		}
	}
}
