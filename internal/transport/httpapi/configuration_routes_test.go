package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"postra/internal/application"
	"postra/internal/domain"
)

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
