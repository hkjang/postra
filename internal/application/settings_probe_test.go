package application

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCandidateAIProbeIsWriteOnlyAndDoesNotChangeLiveSettings(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	old := app.currentAIConfig()
	key := "candidate-private-key-82649"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("candidate credential not sent")
		}
		var request struct {
			Model       string  `json:"model"`
			Temperature float64 `json:"temperature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "candidate-model" || request.Temperature != .4 {
			t.Errorf("wrong candidate config %+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"POSTRA_AI_OK"}}]}`))
	}))
	defer server.Close()
	result, err := app.ProbeSettings(ctx, SettingsProbe{Target: "ai", Values: map[string]string{"ai.base_url": server.URL, "ai.model": "candidate-model", "ai.temperature": "0.4"}, Secrets: map[string]string{"ai.api_key_ref": key}})
	if err != nil || !result.OK {
		t.Fatalf("probe %+v %v", result, err)
	}
	if current := app.currentAIConfig(); current.BaseURL != old.BaseURL || current.Model != old.Model || current.APIKeyRef != old.APIKeyRef {
		t.Fatal("probe mutated live config")
	}
	stored, _ := app.Store.GetSettings(ctx)
	audit, _ := app.Store.SearchAudit(ctx, DefaultUserID, 100)
	for _, value := range []any{stored, audit, result} {
		raw, _ := json.Marshal(value)
		if strings.Contains(string(raw), key) {
			t.Fatal("probe credential exposed")
		}
	}
}

func TestCandidateProviderFailureNeverEchoesCredentials(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	key := "candidate-secret-723496"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`bad credential: ` + key))
	}))
	defer server.Close()
	result, err := app.ProbeSettings(settingsAdmin(), SettingsProbe{Target: "ai", Values: map[string]string{"ai.base_url": server.URL}, Secrets: map[string]string{"ai.api_key_ref": key}})
	if err != nil || result.OK || strings.Contains(result.Message, key) {
		t.Fatalf("unsafe test result: %+v %v", result, err)
	}
}
