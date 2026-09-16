package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

const unsafeProviderText = "opaque-private-provider-value"

func TestModelLimitsSafeFailureReasons(t *testing.T) {
	for _, tt := range []struct {
		name, body, reason string
		status             int
	}{
		{"auth", unsafeProviderText, "auth_failed", 401},
		{"forbidden", unsafeProviderText, "forbidden", 403},
		{"missing_endpoint", unsafeProviderText, "not_supported", 404},
		{"method", unsafeProviderText, "not_supported", 405},
		{"not_implemented", unsafeProviderText, "not_supported", 501},
		{"rate_limit", unsafeProviderText, "rate_limited", 429},
		{"request_timeout", unsafeProviderText, "timeout", 408},
		{"gateway_timeout", unsafeProviderText, "timeout", 504},
		{"server", unsafeProviderText, "server_error", 503},
		{"redirect", unsafeProviderText, "request_rejected", 307},
		{"bad_request", unsafeProviderText, "request_rejected", 400},
		{"bad_json", unsafeProviderText, "invalid_response", 200},
		{"missing_data", `{}`, "invalid_response", 200},
		{"oversized", strings.Repeat("x", (2<<20)+1), "invalid_response", 200},
		{"no_fields", `{"data":[{"id":"chosen"}]}`, "no_metadata", 200},
		{"output_only", `{"data":[{"id":"chosen","max_output_tokens":512}]}`, "no_metadata", 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("X-Error", unsafeProviderText)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			p := NewPreview(limitsConfig(server.URL), nil, unsafeProviderText)
			for range 2 {
				limits, err := p.ModelLimits(context.Background(), "", false)
				if err != nil || limits.Status != "unavailable" || limits.Source != "config" || limits.ContextLength != 32768 || limits.Reason != tt.reason {
					t.Fatalf("wrong fallback: %+v, %v", limits, err)
				}
				encoded, _ := json.Marshal(limits)
				if strings.Contains(string(encoded), unsafeProviderText) || strings.Contains(string(encoded), server.URL) {
					t.Fatalf("metadata leaked private data: %s", encoded)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("negative result bypassed cache: %d", calls.Load())
			}
		})
	}
}

func TestModelLimitsSuccessfulStatesHaveNoFailureReason(t *testing.T) {
	for _, tt := range []struct {
		body, status string
		manual       bool
	}{
		{`{"data":[{"id":"chosen","max_model_len":4096}]}`, "detected", false},
		{`{"data":[]}`, "model_not_found", false},
		{"", "manual", true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tt.body) }))
		cfg := limitsConfig(server.URL)
		cfg.AutoContextLength = !tt.manual
		limits, err := New(cfg, nil).ModelLimits(context.Background(), "", false)
		server.Close()
		if err != nil || limits.Reason != "" || limits.Status != tt.status {
			t.Fatalf("unexpected failure classification: %+v, %v", limits, err)
		}
	}
}

func TestMetadataFailureDoesNotPreventSuccessfulChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, unsafeProviderText)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()
	p := New(limitsConfig(server.URL), nil)
	result, err := p.Generate(context.Background(), domain.GenerationRequest{User: "hello"})
	if err != nil || result.Text != "ok" {
		t.Fatalf("metadata-only failure blocked working generation: %+v, %v", result, err)
	}
	limits, err := p.ModelLimits(context.Background(), "", false)
	if err != nil || limits.Reason != "auth_failed" || limits.ContextLength != 32768 {
		t.Fatalf("metadata reason lost after successful chat: %+v, %v", limits, err)
	}
}

type aiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f aiRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelLimitsNetworkAndBodyReadReasons(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		err          error
	}{
		{"deadline", "timeout", context.DeadlineExceeded},
		{"network", "unreachable", errors.New(unsafeProviderText)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := New(limitsConfig("http://127.0.0.1/v1"), nil)
			p.client.Transport = aiRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, tt.err })
			limits, err := p.ModelLimits(context.Background(), "", false)
			if err != nil || limits.Reason != tt.reason || limits.ContextLength != 32768 {
				t.Fatalf("network diagnostic: %+v, %v", limits, err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"data":[{"id":"chosen","max_model_len":1024}]}`)
	}))
	defer server.Close()
	limits, err := New(limitsConfig(server.URL), nil).ModelLimits(context.Background(), "", false)
	if err != nil || limits.Reason != "invalid_response" || limits.ContextLength != 32768 {
		t.Fatalf("partial metadata accepted: %+v, %v", limits, err)
	}
}

func assertSafeAIError(t *testing.T, err error, code string, status int) {
	t.Helper()
	var public *domain.PublicError
	if !errors.As(err, &public) || public.Code != code || public.Status != status {
		t.Fatalf("error = %v, expected %s/%d", err, code, status)
	}
	if strings.Contains(err.Error(), unsafeProviderText) || strings.Contains(err.Error(), "http://") || public.Details != nil {
		t.Fatalf("unsafe provider error: %+v", public)
	}
}

func TestProviderRejectionsAreClassifiedWithoutFallbackOrSecretExposure(t *testing.T) {
	for _, tt := range []struct {
		name, code, body   string
		status, publicHTTP int
	}{
		{"auth", "ai_auth_failed", "", 401, 502},
		{"permission", "ai_forbidden", "", 403, 502},
		{"rate", "ai_rate_limited", "", 429, 429},
		{"quota_code", "ai_quota_exceeded", `{"error":{"code":"insufficient_quota","message":"` + unsafeProviderText + `"}}`, 429, 429},
		{"quota_type", "ai_quota_exceeded", `{"error":{"type":"insufficient_quota"}}`, 429, 429},
		{"credits", "ai_quota_exceeded", `{"error":{"code":"credit_balance_exhausted"}}`, 429, 429},
		{"organization_spend", "ai_quota_exceeded", `{"error":{"code":"organization_spend_limit_exceeded"}}`, 429, 429},
		{"project_spend", "ai_quota_exceeded", `{"error":{"code":"project_spend_limit_exceeded"}}`, 429, 429},
		{"organization_usage", "ai_quota_exceeded", `{"error":{"code":"organization_usage_limit_exceeded"}}`, 429, 429},
		{"ramp_rate", "ai_rate_limited", `{"error":{"code":"slow_down","type":"rate_limit_error"}}`, 429, 429},
		{"quota_in_message_only", "ai_rate_limited", `{"error":{"message":"insufficient_quota"}}`, 429, 429},
		{"quota_prefix", "ai_rate_limited", `{"error":{"code":"insufficient_quota ` + unsafeProviderText + `"}}`, 429, 429},
		{"quota_wrong_status", "ai_request_rejected", `{"error":{"code":"insufficient_quota"}}`, 400, 502},
		{"untrusted_code", "ai_rate_limited", `{"error":{"code":"` + unsafeProviderText + `"}}`, 429, 429},
		{"bad_request", "ai_request_rejected", "", 400, 502},
		{"unsupported_model", "ai_request_rejected", "", 404, 502},
		{"timeout", "ai_timeout", "", 408, 504},
		{"gateway_timeout", "ai_timeout", "", 504, 504},
		{"redirect", "ai_request_rejected", "", 307, 502},
		{"unhandled_success_status", "ai_request_rejected", "", 202, 502},
	} {
		for _, embed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/embed=%t", tt.name, embed), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(tt.status)
					body := tt.body
					if body == "" {
						body = unsafeProviderText
					}
					_, _ = io.WriteString(w, body)
				}))
				defer server.Close()
				cfg := limitsConfig(server.URL)
				cfg.AutoContextLength = false
				cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task"}}
				p := NewPreview(cfg, nil, unsafeProviderText)
				var err error
				if embed {
					_, err = p.Embed(context.Background(), domain.EmbeddingRequest{Input: []string{"hi"}})
				} else {
					_, err = p.Generate(context.Background(), domain.GenerationRequest{Task: "summarize", User: "hi"})
				}
				assertSafeAIError(t, err, tt.code, tt.publicHTTP)
				if calls.Load() != 1 {
					t.Fatalf("unsafe fallback or retry: %d requests", calls.Load())
				}
			})
		}
	}
}

func TestProviderNetworkFailuresNeverRetryAcceptedGeneration(t *testing.T) {
	for _, embed := range []bool{false, true} {
		for _, tt := range []struct {
			name, code string
			err        error
			status     int
		}{
			{"timeout", "ai_timeout", context.DeadlineExceeded, 504},
			{"connection", "ai_unreachable", errors.New(unsafeProviderText), 502},
			{"cancelled", "", context.Canceled, 0},
		} {
			t.Run(fmt.Sprintf("%s/embed=%t", tt.name, embed), func(t *testing.T) {
				cfg := limitsConfig("http://127.0.0.1/v1")
				cfg.AutoContextLength = false
				cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task"}}
				p := New(cfg, nil)
				var calls atomic.Int32
				p.client.Transport = aiRoundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, tt.err })
				var err error
				if embed {
					_, err = p.Embed(context.Background(), domain.EmbeddingRequest{Input: []string{"hi"}})
				} else {
					_, err = p.Generate(context.Background(), domain.GenerationRequest{Task: "summarize"})
				}
				if tt.code == "" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", err)
					}
				} else {
					assertSafeAIError(t, err, tt.code, tt.status)
				}
				if calls.Load() != 1 {
					t.Fatalf("network failure retried: %d", calls.Load())
				}
			})
		}
	}
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select { // The provider has already accepted the POST.
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	cfg := limitsConfig(server.URL)
	cfg.AutoContextLength = false
	cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task"}}
	p := New(cfg, nil)
	p.client.Timeout = 100 * time.Millisecond
	_, err := p.Generate(context.Background(), domain.GenerationRequest{Task: "summarize"})
	assertSafeAIError(t, err, "ai_timeout", 504)
	if calls.Load() != 1 {
		t.Fatalf("accepted generation retried: %d", calls.Load())
	}
}

func TestServerRejectionStillAllowsTaskDefaultFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request chatRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Model == "task" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, unsafeProviderText)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"default response"}}]}`)
	}))
	defer server.Close()
	cfg := limitsConfig(server.URL)
	cfg.AutoContextLength = false
	cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task"}}
	result, err := New(cfg, nil).Generate(context.Background(), domain.GenerationRequest{Task: "summarize"})
	if err != nil || result.Text != "default response" || result.Model != "chosen" || calls.Load() != 2 {
		t.Fatalf("configured fallback lost: %+v, %v, calls=%d", result, err, calls.Load())
	}
	for _, status := range []int{500, 502, 503} {
		err := providerHTTPError(status, []byte(unsafeProviderText))
		var public *domain.PublicError
		if err == nil || errors.As(err, &public) || strings.Contains(err.Error(), unsafeProviderText) {
			t.Fatalf("server rejection classification or secret leakage: %v", err)
		}
	}
}

type failedAICredentials struct{ domain.SecretStore }

func (failedAICredentials) Acquire(context.Context, domain.SecretRef, domain.SecretPurpose) (*domain.SecretHandle, error) {
	return nil, errors.New(unsafeProviderText)
}

func TestCredentialFailureNeverFallsBackOrExposesReferences(t *testing.T) {
	cfg := limitsConfig("http://127.0.0.1/v1")
	cfg.AutoContextLength = false
	cfg.TaskModels = map[string]config.AITaskRoute{"summarize": {Model: "task", APIKeyRef: unsafeProviderText}}
	p := New(cfg, nil)
	var calls atomic.Int32
	p.client.Transport = aiRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected request")
	})
	_, err := p.Generate(context.Background(), domain.GenerationRequest{Task: "summarize"})
	assertSafeAIError(t, err, "ai_auth_failed", 502)
	if calls.Load() != 0 {
		t.Fatalf("credential failure invoked fallback: %d", calls.Load())
	}
	for _, headers := range []bool{false, true} {
		cfg.TaskModels = nil
		cfg.APIKeyRef, cfg.ExtraHeadersRef = unsafeProviderText, ""
		if headers {
			cfg.APIKeyRef, cfg.ExtraHeadersRef = "", unsafeProviderText
		}
		p := New(cfg, failedAICredentials{})
		_, err := p.Embed(context.Background(), domain.EmbeddingRequest{Input: []string{"hello"}})
		assertSafeAIError(t, err, "ai_auth_failed", 502)
	}
}
