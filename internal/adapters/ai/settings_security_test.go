package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

func TestProviderHonorsTemperatureContextAndDisabledModels(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Temperature != .7 || request.MaxTokens != 32 {
			t.Errorf("wrong policy %+v", request)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	cfg := config.Default().AI
	cfg.BaseURL = server.URL
	cfg.Temperature = .7
	cfg.MaxTokens = 32
	provider := New(cfg, nil)
	if _, err := provider.Generate(context.Background(), domain.GenerationRequest{User: "hello", MaxTokens: 100}); err != nil {
		t.Fatal(err)
	}
	cfg.DisabledModels = []string{cfg.Model}
	provider.Configure(cfg)
	if _, err := provider.Generate(context.Background(), domain.GenerationRequest{User: "hello"}); err == nil {
		t.Fatal("disabled model called")
	}
	cfg.DisabledModels = nil
	cfg.ContextLength = 128
	provider.Configure(cfg)
	if _, err := provider.Generate(context.Background(), domain.GenerationRequest{User: strings.Repeat("한글", 100)}); err == nil {
		t.Fatal("context budget bypass")
	}
	if calls != 1 {
		t.Fatalf("policy errors still called provider %d times", calls)
	}
}

func TestProviderRedirectCannotExfiltrateCandidateKey(t *testing.T) {
	called := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) }))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	cfg := config.Default().AI
	cfg.BaseURL = origin.URL
	_, err := NewPreview(cfg, nil, "candidate-key-9425").Generate(context.Background(), domain.GenerationRequest{User: "private mail"})
	if err == nil || called {
		t.Fatal("AI POST followed unexpected redirect")
	}
}

func TestHeaderSecretsRedactedBeforeTruncation(t *testing.T) {
	secret := strings.Repeat("opaque-private-header", 30)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte("error: " + r.Header.Get("X-Gateway")))
	}))
	defer server.Close()
	cfg := config.Default().AI
	cfg.BaseURL = server.URL
	cfg.ExtraHeaders = `{"X-Gateway":"` + secret + `"}`
	_, err := New(cfg, nil).Generate(context.Background(), domain.GenerationRequest{User: "hi"})
	if err == nil || strings.Contains(err.Error(), "opaque-private-header") {
		t.Fatalf("header leaked: %v", err)
	}
}
