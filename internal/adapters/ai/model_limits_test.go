package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

func limitsConfig(baseURL string) config.AIConfig {
	cfg := config.Default().AI
	cfg.BaseURL, cfg.Model = baseURL, "chosen"
	cfg.ContextLength, cfg.MaxTokens = 32768, 4096
	cfg.AutoContextLength = true
	return cfg
}

func TestModelLimitsMetadataAndFallbacks(t *testing.T) {
	tests := []struct {
		name, response, status string
		code, context, output  int
	}{
		{"vllm", `{"data":[{"id":"other","max_model_len":99999},{"id":"chosen","max_model_len":2048,"max_output_tokens":256}]}`, "detected", 200, 2048, 256},
		{"nested", `{"data":[{"id":"chosen","context_length":8192,"model_info":{"context_window":"4096","max_completion_tokens":1024}}]}`, "detected", 200, 4096, 1024},
		{"standard_no_metadata", `{"data":[{"id":"chosen","created":123,"owned_by":"vllm"}]}`, "unavailable", 200, 32768, 4096},
		{"wrong_model", `{"data":[{"id":"other","max_model_len":1024}]}`, "model_not_found", 200, 32768, 4096},
		{"training_limit_not_runtime", `{"data":[{"id":"chosen","meta":{"n_ctx_train":131072},"max_position_embeddings":131072}]}`, "unavailable", 200, 32768, 4096},
		{"invalid_limits", `{"data":[{"id":"chosen","max_model_len":-1,"context_length":1.5,"max_context_length":999999999999,"context_window":"unlimited"}]}`, "unavailable", 200, 32768, 4096},
		{"output_only", `{"data":[{"id":"chosen","max_output_tokens":512}]}`, "unavailable", 200, 32768, 512},
		{"404", `not supported`, "unavailable", 404, 32768, 4096},
		{"401", `{"error":"api_key=sk-private-not-exposed"}`, "unavailable", 401, 32768, 4096},
		{"malformed", `not json`, "unavailable", 200, 32768, 4096},
		{"no_data", `{"object":"list"}`, "unavailable", 200, 32768, 4096},
		{"empty_list", `{"data":[]}`, "model_not_found", 200, 32768, 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
					t.Errorf("unexpected metadata path %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()
			cfg := limitsConfig(server.URL + "/v1/")
			p := New(cfg, nil)
			for range 2 {
				limits, err := p.ModelLimits(context.Background(), "", false)
				if err != nil {
					t.Fatal(err)
				}
				if limits.Model != "chosen" || limits.ContextLength != tt.context || limits.MaxOutputTokens != tt.output || limits.Status != tt.status {
					t.Fatalf("limits %+v", limits)
				}
				expectedSource := "config"
				if tt.status == "detected" {
					expectedSource = "models"
				}
				if limits.Source != expectedSource {
					t.Fatalf("source %s", limits.Source)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("positive/negative cache bypass: %d", calls.Load())
			}
		})
	}
}

func TestModelLimitsManualDisabledAndRedirect(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	cfg := limitsConfig(server.URL)
	cfg.AutoContextLength = false
	p := New(cfg, nil)
	limits, err := p.ModelLimits(context.Background(), "", false)
	if err != nil || limits.Status != "manual" || limits.ContextLength != cfg.ContextLength || calls.Load() != 0 {
		t.Fatalf("manual %+v %v calls=%d", limits, err, calls.Load())
	}
	cfg.AutoContextLength = true
	cfg.DisabledModels = []string{cfg.Model}
	p.Configure(cfg)
	if _, err = p.ModelLimits(context.Background(), "", false); err == nil || calls.Load() != 0 {
		t.Fatalf("disabled model contacted %v", err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	cfg = limitsConfig(origin.URL)
	p = NewPreview(cfg, nil, "candidate-private-key")
	limits, err = p.ModelLimits(context.Background(), "", false)
	if err != nil || limits.Status != "unavailable" || calls.Load() != 0 {
		t.Fatalf("redirect followed %+v %v", limits, err)
	}
}

func TestModelLimitsConcurrentConfigureAndExpiry(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"chosen","max_model_len":4096},{"id":"second","max_model_len":2048}]}`))
	}))
	defer server.Close()
	cfg := limitsConfig(server.URL)
	p := New(cfg, nil)
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Configure(cfg)
			limits, err := p.ModelLimits(context.Background(), "", false)
			if err != nil || limits.ContextLength != 4096 {
				t.Errorf("concurrent lookup %+v %v", limits, err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("lookup not coalesced %d", calls.Load())
	}
	key := modelLimitsKey(cfg, cfg.RouteForTask(""), "", "")
	p.limitsMu.Lock()
	cached := p.limitsCache[key]
	cached.expires = time.Now().Add(-time.Second)
	p.limitsCache[key] = cached
	p.limitsMu.Unlock()
	_, _ = p.ModelLimits(context.Background(), "", false)
	if calls.Load() != 2 {
		t.Fatalf("expired cache not refreshed %d", calls.Load())
	}
	cfg.Model = "second"
	p.Configure(cfg)
	limits, err := p.ModelLimits(context.Background(), "", false)
	if err != nil || limits.ContextLength != 2048 || calls.Load() != 3 {
		t.Fatalf("stale model %+v %v calls=%d", limits, err, calls.Load())
	}
}

func TestModelLimitsRoutesCredentialsAndCacheIdentity(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer candidate-private-key" {
			t.Error("metadata credential missing")
		}
		if r.Header.Get("X-Gateway") == "" {
			t.Error("gateway credential missing")
		}
		switch r.URL.Path {
		case "/default/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chosen","max_model_len":4096}]}`))
		case "/task/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"task","max_model_len":2048,"max_output_tokens":256}]}`))
		case "/embed/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"embed","max_model_len":1024}]}`))
		default:
			t.Errorf("wrong route %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := limitsConfig(server.URL + "/default")
	cfg.ExtraHeaders = `{"X-Gateway":"gateway-one"}`
	cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task", BaseURL: server.URL + "/task", MaxTokens: 128}}
	cfg.EmbedBaseURL, cfg.EmbedModel = server.URL+"/embed", "embed"
	p := NewPreview(cfg, nil, "candidate-private-key")
	for _, tt := range []struct {
		task            string
		embed           bool
		context, output int
	}{{"", false, 4096, 4096}, {"summarize", false, 2048, 128}, {"", true, 1024, 0}} {
		limits, err := p.ModelLimits(context.Background(), tt.task, tt.embed)
		if err != nil || limits.ContextLength != tt.context || limits.MaxOutputTokens != tt.output {
			t.Fatalf("route %+v %+v %v", tt, limits, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("routes mixed %d", calls.Load())
	}
	cfg.APIKeyRef = "connection-preview-only"
	cfg.ExtraHeaders = `{"X-Gateway":"gateway-two"}`
	p.Configure(cfg)
	_, _ = p.ModelLimits(context.Background(), "", false)
	if calls.Load() != 4 {
		t.Fatalf("gateway change retained old cache %d", calls.Load())
	}
	first := modelLimitsKey(cfg, cfg.RouteForTask(""), "old-key", cfg.ExtraHeaders)
	second := modelLimitsKey(cfg, cfg.RouteForTask(""), "new-key", cfg.ExtraHeaders)
	if first == second {
		t.Fatal("secret rotation omitted from cache identity")
	}
}

func TestGenerateAutomaticallyBudgetsOutputWithoutChangingMail(t *testing.T) {
	input := "사용자 요청\n" + strings.Repeat("안녕하세요 ", 40)
	var received chatRequest
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"chosen","max_model_len":2048,"max_output_tokens":1900}]}`))
			return
		}
		posts.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&received)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	cfg := limitsConfig(server.URL)
	p := New(cfg, nil)
	_, err := p.Generate(context.Background(), domain.GenerationRequest{System: "trusted", User: input, Untrusted: "원본 이메일", MaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if received.MaxTokens != 2048-estimatePromptTokens(received.Messages) || received.MaxTokens >= 1900 {
		t.Fatalf("output budget %d", received.MaxTokens)
	}
	if received.Messages[0].Content != "trusted\n\n"+guardrail || received.Messages[1].Content != input+"\n\n"+untrustedBlock("원본 이메일") {
		t.Fatal("prompt/guardrail was truncated")
	}
	_, err = p.Generate(context.Background(), domain.GenerationRequest{User: strings.Repeat("한글", 1000)})
	var public *domain.PublicError
	if !errors.As(err, &public) || public.Code != "context_limit" || posts.Load() != 1 {
		t.Fatalf("oversized input sent: %v calls=%d", err, posts.Load())
	}
}

func TestGenerateExplicitRequestCannotExceedTaskOutputCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		var request chatRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.MaxTokens != 64 {
			t.Errorf("task cap ignored %d", request.MaxTokens)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	cfg := limitsConfig(server.URL)
	cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {MaxTokens: 64}}
	_, err := New(cfg, nil).Generate(context.Background(), domain.GenerationRequest{Task: "summarize", User: "hello", MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGenerateBoundedSafeRejections(t *testing.T) {
	tests := []struct {
		name, rejection                     string
		code, expectedCalls, expectedOutput int
		completionParameter, wantError      bool
	}{
		{"provider_exact_context", `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 1024 tokens. However, you requested 4996 tokens (900 in the messages, 4096 in the completion)."}}`, 400, 2, 124, false, false},
		{"vllm_context", `{"error":{"message":"max_tokens is too large: 4096. This model's maximum context length is 1024 tokens and your request has 900 input tokens."}}`, 400, 2, 124, false, false},
		{"output_cap", `{"error":{"message":"max_tokens must be less than or equal to 256"}}`, 422, 2, 256, false, false},
		{"completion_parameter", `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`, 400, 2, 4096, true, false},
		{"input_alone_too_long", `{"error":{"code":"context_length_exceeded","message":"maximum context length is 512 tokens; request has 900 input tokens"}}`, 400, 1, 0, false, true},
		{"no_counts", `{"error":{"code":"context_length_exceeded","message":"input too long"}}`, 400, 1, 0, false, true},
		{"server_error_not_retried", `{"error":{"message":"maximum context length is 1024 tokens; request has 900 input tokens"}}`, 500, 1, 0, false, true},
		{"unauthorized_not_retried", `{"error":{"message":"max_tokens must be less than or equal to 256"}}`, 401, 1, 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			var original []chatMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					http.NotFound(w, r)
					return
				}
				calls++
				var request chatRequest
				_ = json.NewDecoder(r.Body).Decode(&request)
				if calls == 1 {
					original = request.Messages
					w.WriteHeader(tt.code)
					_, _ = w.Write([]byte(tt.rejection))
					return
				}
				if !reflect.DeepEqual(original, request.Messages) {
					t.Error("retry changed prompt")
				}
				if minPositive(request.MaxTokens, request.MaxCompletionTokens) != tt.expectedOutput || (request.MaxCompletionTokens > 0) != tt.completionParameter {
					t.Errorf("retry body %+v", request)
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			}))
			defer server.Close()
			_, err := New(limitsConfig(server.URL), nil).Generate(context.Background(), domain.GenerationRequest{User: "hello"})
			if (err != nil) != tt.wantError || calls != tt.expectedCalls {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestLengthFinishNeverReturnsPartialJSONOrFallsBack(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					http.NotFound(w, r)
					return
				}
				calls.Add(1)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n"))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`))
			}))
			defer server.Close()
			cfg := limitsConfig(server.URL)
			cfg.Stream = stream
			cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task"}}
			result, err := New(cfg, nil).Generate(context.Background(), domain.GenerationRequest{Task: "summarize", JSONMode: true})
			var public *domain.PublicError
			if !errors.As(err, &public) || public.Code != "output_limit" || result.Text != "" || calls.Load() != 1 {
				t.Fatalf("truncated result %+v %v calls=%d", result, err, calls.Load())
			}
		})
	}
}

func TestModelLimitsCacheIsBounded(t *testing.T) {
	p := New(config.AIConfig{}, nil)
	p.limitsMu.Lock()
	defer p.limitsMu.Unlock()
	for i := range 1000 {
		var key [32]byte
		key[0], key[1] = byte(i), byte(i>>8)
		p.cacheModelLimitsLocked(key, discoveredModelLimits{context: 1024, status: "detected"})
	}
	if len(p.limitsCache) != modelLimitsCacheSize {
		t.Fatalf("unbounded cache: %d", len(p.limitsCache))
	}
}

func TestCancelledMetadataCallerDoesNotPoisonConcurrentLookup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"data":[{"id":"chosen","max_model_len":2048}]}`))
	}))
	defer server.Close()
	p := New(limitsConfig(server.URL), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() { _, err := p.ModelLimits(ctx, "", false); cancelled <- err }()
	<-started
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("cancellation lost %v", err)
	}
	close(release)
	limits, err := p.ModelLimits(context.Background(), "", false)
	if err != nil || limits.ContextLength != 2048 || calls.Load() != 1 {
		t.Fatalf("shared lookup poisoned %+v %v calls=%d", limits, err, calls.Load())
	}
}

func TestMetadataTimeoutFallsBackAndIsNegativeCached(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-r.Context().Done() }))
	defer server.Close()
	p := New(limitsConfig(server.URL), nil)
	p.client = &http.Client{Timeout: 20 * time.Millisecond, CheckRedirect: rejectAIRedirect}
	for range 2 {
		limits, err := p.ModelLimits(context.Background(), "", false)
		if err != nil || limits.Status != "unavailable" || limits.ContextLength != 32768 {
			t.Fatalf("timeout fallback %+v %v", limits, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("timeout was not negative cached %d", calls.Load())
	}
}

func TestRejectedRequestRetriesAtMostOnceAndRedactsCredentials(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"max_tokens must be less than or equal to 256; candidate-private-key gateway-secret"}}`))
	}))
	defer server.Close()
	cfg := limitsConfig(server.URL)
	cfg.ExtraHeaders = `{"X-Gateway":"gateway-secret"}`
	_, err := NewPreview(cfg, nil, "candidate-private-key").Generate(context.Background(), domain.GenerationRequest{User: "hello"})
	if err == nil || calls.Load() != 2 || strings.Contains(err.Error(), "candidate-private-key") || strings.Contains(err.Error(), "gateway-secret") {
		t.Fatalf("unsafe retry/error calls=%d err=%v", calls.Load(), err)
	}
}

func TestIncompleteAIResponsesAreNotUsedOrRetried(t *testing.T) {
	for _, body := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\ndata: not-json\ndata: [DONE]\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\ndata: {\"error\":{\"message\":\"secret upstream error\"}}\n",
	} {
		t.Run(body, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					http.NotFound(w, r)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(body)+100)) // Broken stream, not a clean transport EOF.
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			cfg := limitsConfig(server.URL)
			cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task"}}
			result, err := New(cfg, nil).Generate(context.Background(), domain.GenerationRequest{Task: "summarize"})
			var public *domain.PublicError
			if !errors.As(err, &public) || public.Code != "invalid_response" || result.Text != "" || calls.Load() != 1 {
				t.Fatalf("partial response accepted/retried %+v %v calls=%d", result, err, calls.Load())
			}
		})
	}
}

func TestCancelledGenerationDoesNotInvokeFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cfg := limitsConfig(server.URL)
	cfg.AutoContextLength = false
	cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task"}}
	_, err := New(cfg, nil).Generate(ctx, domain.GenerationRequest{Task: "summarize"})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("cancelled call retried %v calls=%d", err, calls.Load())
	}
}
