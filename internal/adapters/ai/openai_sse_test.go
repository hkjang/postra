package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

func TestParseSSEChatStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{"content":" world"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	got, err := parseSSEChatStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != "Hello world" {
		t.Fatalf("got %q, want %q", got, "Hello world")
	}
}

func TestParseSSEChatStreamError(t *testing.T) {
	sse := "data: {\"error\":{\"message\":\"bad model\"}}\n\n"
	if _, err := parseSSEChatStream(strings.NewReader(sse)); err == nil {
		t.Fatal("expected error from in-band error frame")
	}
}

// TestGenerateStreaming verifies Generate consumes a text/event-stream
// response when the server honors stream=true.
func TestGenerateStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"POSTRA_AI_OK\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := New(config.AIConfig{BaseURL: srv.URL, Model: "test", Stream: true, TimeoutSec: 5}, nil)
	res, err := p.Generate(context.Background(), domain.GenerationRequest{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.Text != "POSTRA_AI_OK" {
		t.Fatalf("got %q", res.Text)
	}
}
