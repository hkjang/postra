package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"postra/internal/domain"
)

func TestModelLimitProbeRoutesWithoutGeneratingOrSaving(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	const taskKey = "task-probe-private-credential"
	ref, err := app.RegisterSecret(ctx, domain.SecretAPIKey, "task probe", domain.NewSecretHandle([]byte(taskKey)))
	if err != nil {
		t.Fatal(err)
	}
	var modelRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("metadata probe generated content: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+taskKey {
			t.Error("task credential was replaced by global candidate key")
		}
		modelRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"wrong-model","max_model_len":128},{"id":"summary-model","max_model_len":131072,"max_output_tokens":4096}]}`))
	}))
	defer server.Close()
	routes, _ := json.Marshal(map[string]any{"summarize": map[string]any{"base_url": server.URL + "/v1", "model": "summary-model", "api_key_ref": string(ref), "max_tokens": 1024}})
	before, _ := app.Store.GetSettings(ctx)
	result, err := app.ProbeSettings(ctx, SettingsProbe{Target: "ai_models", Task: "summarize", Values: map[string]string{"ai.base_url": "http://unused.invalid/v1", "ai.task_models": string(routes)}, Secrets: map[string]string{"ai.api_key_ref": "global-candidate-must-not-leave"}})
	if err != nil || !result.OK || result.Model != "summary-model" || result.Limits == nil || result.Limits.ContextLength != 131072 || result.Limits.MaxOutputTokens != 1024 || result.Limits.Status != "detected" {
		t.Fatalf("wrong route limits: %+v limits=%+v err=%v", result, result.Limits, err)
	}
	if modelRequests.Load() != 1 {
		t.Fatal("model metadata not requested exactly once")
	}
	after, _ := app.Store.GetSettings(ctx)
	oldJSON, _ := json.Marshal(before)
	newJSON, _ := json.Marshal(after)
	if string(oldJSON) != string(newJSON) {
		t.Fatal("metadata probe persisted candidate configuration")
	}
	audit, _ := app.Store.SearchAudit(ctx, DefaultUserID, 20)
	for _, output := range []any{result, after, audit} {
		data, _ := json.Marshal(output)
		if strings.Contains(string(data), taskKey) || strings.Contains(string(data), "global-candidate-must-not-leave") {
			t.Fatal("metadata probe exposed credentials")
		}
	}
}

func TestModelLimitProbeFallbackManualAndEmbedding(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			t.Error("models-only probe made generation request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"embed-small","max_model_len":2048}]}`))
	}))
	defer server.Close()
	for _, test := range []struct {
		name, target, model, automatic, status string
		length                                 int
		ok                                     bool
	}{
		{"embedding", "embedding_models", "embed-small", "true", "detected", 2048, true},
		{"missing model", "ai_models", "absent-model", "true", "model_not_found", 8192, false},
		{"manual", "ai_models", "manual-model", "false", "manual", 8192, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := calls.Load()
			result, err := app.ProbeSettings(settingsAdmin(), SettingsProbe{Target: test.target, Values: map[string]string{"ai.base_url": server.URL, "ai.embed_base_url": server.URL, "ai.model": test.model, "ai.embed_model": test.model, "ai.context_length": "8192", "ai.auto_context_length": test.automatic}})
			if err != nil || result.OK != test.ok || result.Limits == nil || result.Limits.ContextLength != test.length || result.Limits.Status != test.status || result.Model != test.model {
				t.Fatalf("result %+v limits=%+v err=%v", result, result.Limits, err)
			}
			if test.automatic == "false" && calls.Load() != before {
				t.Fatal("manual mode contacted model server")
			}
		})
	}
	user := WithPrincipal(context.Background(), domain.Principal{UserID: "ordinary-user", Role: domain.RoleUser})
	before := calls.Load()
	if _, err := app.ProbeSettings(user, SettingsProbe{Target: "ai_models"}); err == nil || calls.Load() != before {
		t.Fatal("model probes require administrator access")
	}
}

func TestModelLimitProbeUnsupportedDoesNotExposeServerResponse(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("sensitive-upstream-model-list-error"))
	}))
	defer server.Close()
	result, err := app.ProbeSettings(settingsAdmin(), SettingsProbe{Target: "ai_models", Values: map[string]string{"ai.base_url": server.URL}})
	if err != nil || result.OK || result.Limits == nil || result.Limits.Status != "unavailable" || !strings.Contains(result.Message, "대체 한도") || strings.Contains(result.Message, "sensitive-upstream") {
		t.Fatalf("unexpected fallback %+v %v", result, err)
	}
}

func TestAIProbeReportsOutputLimitInsteadOfAuthenticationFailure(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"reasoning-model","max_model_len":8192}]}`))
			return
		}
		var request struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.MaxTokens != 256 {
			t.Errorf("incorrect probe output budget: %+v %v", request, err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"private partial output"},"finish_reason":"length"}]}`))
	}))
	defer server.Close()
	result, err := app.ProbeSettings(settingsAdmin(), SettingsProbe{Target: "ai", Values: map[string]string{"ai.base_url": server.URL, "ai.model": "reasoning-model"}})
	if err != nil || result.OK || result.Message != providerAIOutput || result.Limits == nil {
		t.Fatalf("length failure misclassified %+v %v", result, err)
	}
}
