package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"postra/internal/domain"
	"postra/internal/platform/tracking"
	"strings"
	"testing"
)

func TestTrackingFrameIsolationAndAdminPermission(t *testing.T) {
	app := browserTestApp(t, true)
	admin, csrf := browserIdentity(t, app, "tracking-admin", domain.RoleAdmin)
	member, _ := browserIdentity(t, app, "tracking-member", domain.RoleUser)
	if err := app.Store.UpsertSettings(context.Background(), map[string]string{tracking.SettingEnabled: "true", tracking.SettingProvider: "custom", tracking.SettingCustomSnippet: `<script src="https://collector.test/tracker.js"></script>`, tracking.SettingIncludeAdmin: "false"}); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()
	frame := trackingFramePath + "?page=" + url.QueryEscape("/app/messages/:id")
	if w := browserRequest(h, "GET", frame, "", "", "", ""); w.Code != 401 {
		t.Fatalf("anonymous frame: %d", w.Code)
	}
	w := browserRequest(h, "GET", frame, member, "", "", "")
	policy := w.Header().Get("Content-Security-Policy")
	if w.Code != 200 || !strings.Contains(policy, "sandbox allow-scripts;") || strings.Contains(policy, "allow-same-origin") || strings.Contains(policy, "'unsafe-inline'") || !strings.Contains(policy, "frame-ancestors 'self'") || !strings.Contains(w.Body.String(), `nonce="`) {
		t.Fatalf("tracker not isolated: %d %s", w.Code, policy)
	}
	if strings.Contains(w.Body.String(), member) || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("credential/referrer exposure")
	}
	for _, page := range []string{"/app/login", "/app/setup", "/app/settings", "/app/keys", "/app/messages/private-id", "/app/mail?q=private", "/app/admin"} {
		w := browserRequest(h, "GET", trackingFramePath+"?page="+url.QueryEscape(page), member, "", "", "")
		if w.Code != 404 {
			t.Fatalf("sensitive/excluded tracking page accepted: %q %d", page, w.Code)
		}
	}
	body := `{"csp-report":{"blocked-uri":"https://blocked.test/path?secret=never","effective-directive":"connect-src","document-uri":"https://postra.test/api/tracking/frame?page=%2Fapp%2Fmail&secret=never"}}`
	if w := browserRequest(h, "POST", trackingReportPath, "", "", "", body); w.Code != 204 {
		t.Fatalf("CSP report %d", w.Code)
	}
	if w := browserRequest(h, "GET", "/api/admin/tracking", member, "", "", ""); w.Code != 403 {
		t.Fatalf("member read tracking admin: %d", w.Code)
	}
	w = browserRequest(h, "GET", "/api/admin/tracking", admin, "", "", "")
	if !strings.Contains(w.Body.String(), "https://blocked.test") || strings.Contains(w.Body.String(), "never") || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("report leaked URL details: %s", w.Body.String())
	}
	if w := browserRequest(h, "POST", "/api/admin/tracking/allow", admin, "", "https://postra.test", `{"origin":"https://blocked.test"}`); w.Code != 403 {
		t.Fatalf("allow omitted CSRF: %d", w.Code)
	}
	if w := browserRequest(h, "POST", "/api/admin/tracking/allow", admin, csrf, "https://postra.test", `{"origin":"https://blocked.test"}`); w.Code != 200 {
		t.Fatalf("allow failed: %d %s", w.Code, w.Body.String())
	}
	if w := browserRequest(h, "POST", "/api/admin/tracking/allow", admin, csrf, "https://postra.test", `{"origin":"javascript:alert(1)"}`); w.Code != 400 {
		t.Fatalf("unsafe tracking origin accepted: %d", w.Code)
	}
	if w := browserRequest(h, "DELETE", "/api/admin/tracking/violations", admin, csrf, "https://postra.test", ""); w.Code != 200 || strings.Contains(w.Body.String(), "blocked.test") {
		t.Fatalf("forget failed: %d %s", w.Code, w.Body.String())
	}
}

func TestTrackingConfigurationRejectsOversizedOrIncompleteProvider(t *testing.T) {
	app := browserTestApp(t, true)
	admin, csrf := browserIdentity(t, app, "tracking-config-admin", domain.RoleAdmin)
	h := New(app, "").Handler()
	patch := func(values map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(map[string]any{"values": values})
		if err != nil {
			t.Fatal(err)
		}
		return browserRequest(h, "PATCH", "/api/admin/configuration", admin, csrf, "https://postra.test", string(data))
	}
	oversized := "<script>" + strings.Repeat("x", tracking.MaxSnippetBytes) + "</script>"
	response := patch(map[string]string{tracking.SettingEnabled: "true", tracking.SettingProvider: "custom", tracking.SettingCustomSnippet: oversized})
	if response.Code != 400 || strings.Contains(response.Body.String(), strings.Repeat("x", 50)) {
		t.Fatalf("oversized snippet not safely rejected: %d", response.Code)
	}
	stored, err := app.Store.GetSettings(context.Background())
	if err != nil || stored[tracking.SettingCustomSnippet] != "" {
		t.Fatal("oversized snippet was persisted")
	}
	if response := patch(map[string]string{tracking.SettingEnabled: "true", tracking.SettingProvider: "momento"}); response.Code != 400 {
		t.Fatalf("incomplete active provider accepted: %d", response.Code)
	}
	if response := patch(map[string]string{tracking.SettingEnabled: "false", tracking.SettingProvider: "momento", tracking.SettingMomentoURL: "https://collector.test"}); response.Code != 200 {
		t.Fatalf("disabled partial configuration rejected: %d %s", response.Code, response.Body.String())
	}
}

func TestTrackingProxyStripsCredentialsAndCollectorCookies(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Cookie", "Authorization", "X-CSRF-Token", "Referer", "Proxy-Authorization"} {
			if r.Header.Get(name) != "" {
				t.Errorf("collector received %s", name)
			}
		}
		if r.URL.Path != "/collector/events" {
			t.Errorf("proxy path %q", r.URL.Path)
		}
		http.SetCookie(w, &http.Cookie{Name: "collector-session", Value: "never-store"})
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()
	app := browserTestApp(t, false)
	if err := app.Store.UpsertSettings(context.Background(), map[string]string{tracking.SettingEnabled: "true", tracking.SettingProvider: "momento", tracking.SettingMomentoURL: collector.URL + "/collector", tracking.SettingMomentoSiteID: "fixture-site", tracking.SettingMomentoProxy: "true"}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "https://postra.test/momento/events", strings.NewReader(`{"page":"/app/mail"}`))
	for _, name := range []string{"Cookie", "Authorization", "X-CSRF-Token", "Referer", "Proxy-Authorization"} {
		r.Header.Set(name, "fixture-not-forwarded")
	}
	w := httptest.NewRecorder()
	New(app, "").Handler().ServeHTTP(w, r)
	if w.Code != 202 || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Access-Control-Allow-Credentials") != "" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("unsafe proxy response %d %+v", w.Code, w.Header())
	}
}
