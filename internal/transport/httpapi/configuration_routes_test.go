package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"postra/internal/application"
	"postra/internal/domain"
)

func TestAIModelLimitProbeAliasesEnforceAdminAndCSRF(t *testing.T) {
	app := browserTestApp(t, true)
	admin, csrf := browserIdentity(t, app, "model-admin", domain.RoleAdmin)
	user, userCSRF := browserIdentity(t, app, "model-user", domain.RoleUser)
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer write-only-probe-key" {
			t.Error("candidate credential not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"actual-model","max_model_len":16000}]}`))
	}))
	defer modelServer.Close()
	body, _ := json.Marshal(application.SettingsProbe{Target: "ai_models", Values: map[string]string{"ai.base_url": modelServer.URL + "/v1", "ai.model": "actual-model"}, Secrets: map[string]string{"ai.api_key_ref": "write-only-probe-key"}})
	h := New(app, "").Handler()
	for _, prefix := range []string{"/api", "/api/v1"} {
		path := prefix + "/admin/configuration/test"
		before := calls.Load()
		for _, attempt := range []struct{ identity, token, origin string }{{"", "", "https://postra.test"}, {user, userCSRF, "https://postra.test"}, {admin, "", "https://evil.test"}} {
			w := browserRequest(h, "POST", path, attempt.identity, attempt.token, attempt.origin, string(body))
			if w.Code != 401 && w.Code != 403 {
				t.Fatalf("unprivileged model probe allowed: %d", w.Code)
			}
		}
		if calls.Load() != before {
			t.Fatal("forbidden probes contacted model server")
		}
		w := browserRequest(h, "POST", path, admin, csrf, "https://postra.test", string(body))
		var result application.AIConnectionResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.OK || result.Limits == nil || result.Limits.ContextLength != 16000 || result.Limits.Source != "models" {
			t.Fatalf("wrong probe contract: %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "write-only-probe-key") {
			t.Fatal("probe credential exposed")
		}
	}
}

func TestAIAuthenticationDiagnosticsDoNotExpireBrowserSession(t *testing.T) {
	app := browserTestApp(t, true)
	admin, csrf := browserIdentity(t, app, "ai-diagnostic-admin", domain.RoleAdmin)
	const privateResponse = "private-upstream-credential-echo"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(privateResponse))
	}))
	defer server.Close()
	h := New(app, "").Handler()
	for _, prefix := range []string{"/api", "/api/v1"} {
		for _, target := range []string{"ai_models", "ai"} {
			body, _ := json.Marshal(application.SettingsProbe{Target: target, Values: map[string]string{"ai.base_url": server.URL}})
			w := browserRequest(h, "POST", prefix+"/admin/configuration/test", admin, csrf, "https://postra.test", string(body))
			var result application.AIConnectionResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.OK || !strings.Contains(result.Message, "인증에 실패") {
				t.Fatalf("upstream authentication misclassified as browser auth: %d %s", w.Code, w.Body.String())
			}
			if target == "ai_models" && (result.Limits == nil || result.Limits.Reason != "auth_failed") {
				t.Fatal("model metadata failure reason missing")
			}
			if strings.Contains(w.Body.String(), privateResponse) {
				t.Fatal("upstream error body disclosed")
			}
			if session := browserRequest(h, "GET", "/auth/session", admin, "", "", ""); session.Code != 200 {
				t.Fatal("AI authentication failure expired the Postra session")
			}
		}
	}
}

func TestConfigurationAPIUsesSameVersionedContractAndCSRF(t *testing.T) {
	app := browserTestApp(t, true)
	admin, csrf := browserIdentity(t, app, "settings-admin", domain.RoleAdmin)
	user, userCSRF := browserIdentity(t, app, "settings-user", domain.RoleUser)
	h := New(app, "").Handler()
	for _, prefix := range []string{"/api", "/api/v1"} {
		w := browserRequest(h, "GET", prefix+"/admin/configuration", admin, "", "", "")
		var view application.SettingsView
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 || len(view.Fields) < 80 {
			t.Fatalf("catalog %s: %d %s", prefix, w.Code, w.Body.String())
		}
		if w.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing trace ID")
		}
		if w := browserRequest(h, "GET", prefix+"/admin/configuration", user, "", "", ""); w.Code != 403 {
			t.Fatalf("nonadmin status %d", w.Code)
		}
		w = browserRequest(h, "PATCH", prefix+"/preferences", user, "", "https://evil.test", `{"values":{"ui.theme":"dark"}}`)
		if w.Code != 403 {
			t.Fatal("CSRF bypass through API alias")
		}
		w = browserRequest(h, "PATCH", prefix+"/preferences", user, userCSRF, "https://postra.test", `{"values":{"ui.theme":"dark"}}`)
		if w.Code != 200 {
			t.Fatalf("personal settings %d %s", w.Code, w.Body.String())
		}
		w = browserRequest(h, "PATCH", prefix+"/admin/configuration", admin, csrf, "https://postra.test", `{"values":{"ai.timeout_sec":"-1"}}`)
		var errbody domain.ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &errbody); err != nil || w.Code != 400 || errbody.Code == "" || errbody.TraceID != w.Header().Get("X-Request-ID") {
			t.Fatalf("shared error contract: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestConfigurationNeverReturnsInternalSettingsOrSecrets(t *testing.T) {
	app := browserTestApp(t, true)
	admin, _ := browserIdentity(t, app, "catalog-admin", domain.RoleAdmin)
	h := New(app, "").Handler()
	for _, path := range []string{"/api/admin/configuration", "/api/admin/settings", "/api/preferences"} {
		w := browserRequest(h, "GET", path, admin, "", "", "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "internal.oidc_state_key") {
			t.Fatalf("settings leak: %s", w.Body.String())
		}
	}
	for _, path := range []string{"/healthz", "/livez", "/readyz", "/api/v1/healthz"} {
		w := browserRequest(h, "GET", path, "", "", "", "")
		if w.Code != 200 {
			t.Fatalf("probe %s: %d", path, w.Code)
		}
	}
}
