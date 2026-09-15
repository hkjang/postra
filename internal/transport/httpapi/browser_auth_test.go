package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
)

func browserTestApp(t *testing.T, enabled bool) *application.App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir, cfg.Auth.Enabled = dir, enabled
	kek, err := crypto.LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := persistence.Open(filepath.Join(dir, "browser.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	local, err := objectstore.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.New(cfg, store, objectstore.NewEncrypted(local, kek), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)
	return app
}

func browserIdentity(t *testing.T, app *application.App, id string, role domain.UserRole) (string, string) {
	t.Helper()
	u := &domain.User{ID: id, LoginID: id, DisplayName: id, Email: id + "@corp.local", Role: role, Status: domain.UserActive, AuthProvider: "local"}
	if err := app.Store.CreateUser(context.Background(), u, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.CreateAccount(context.Background(), &domain.MailAccount{ID: "acc-" + id, UserID: id, Name: id, Email: u.Email, Status: domain.AccountActive}); err != nil {
		t.Fatal(err)
	}
	raw, csrf, _, err := app.CreateSession(context.Background(), u, "browser-test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return raw, csrf
}

func browserRequest(h http.Handler, method, target, session, csrf, origin, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://postra.test"+target, strings.NewReader(body))
	if session != "" {
		r.AddCookie(&http.Cookie{Name: "postra_session", Value: session})
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBrowserSessionUserIsolationAndCSRF(t *testing.T) {
	app := browserTestApp(t, true)
	admin, adminCSRF := browserIdentity(t, app, "admin", domain.RoleAdmin)
	member, memberCSRF := browserIdentity(t, app, "member", domain.RoleUser)
	h := New(app, "").Handler()
	for _, tc := range []struct{ session, id string }{{admin, "admin"}, {member, "member"}} {
		rec := browserRequest(h, "GET", "/api/auth/session", tc.session, "", "", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"user_id":"`+tc.id+`"`) || strings.Contains(rec.Body.String(), tc.session) || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("session bridge leaked/misidentified credentials: %d %s", rec.Code, rec.Body.String())
		}
		rec = browserRequest(h, "GET", "/api/accounts", tc.session, "", "", "")
		var accounts []domain.MailAccount
		if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil || len(accounts) != 1 || accounts[0].UserID != tc.id {
			t.Fatalf("session account isolation failed: %s, %v", rec.Body.String(), err)
		}
	}
	if rec := browserRequest(h, "GET", "/api/accounts/acc-member", admin, "", "", ""); rec.Code != 404 {
		t.Fatalf("admin must not read another user's mail account: %d", rec.Code)
	}
	for _, tc := range []struct{ origin, csrf string }{
		{"https://evil.test", memberCSRF}, {"https://postra.test:444", memberCSRF},
		{"http://postra.test", memberCSRF}, {"null", memberCSRF}, {"", memberCSRF},
		{"https://postra.test", ""}, {"https://postra.test", adminCSRF},
	} {
		rec := browserRequest(h, "POST", "/api/drafts", member, tc.csrf, tc.origin, `{"account_id":"acc-member","subject":"draft","body":"text"}`)
		if rec.Code != 403 {
			t.Fatalf("origin %q csrf check returned %d", tc.origin, rec.Code)
		}
	}
	rec := browserRequest(h, "POST", "/api/drafts", member, memberCSRF, "https://postra.test", `{"account_id":"acc-member","subject":"draft","body":"text"}`)
	if rec.Code != 201 {
		t.Fatalf("valid browser mutation failed: %d %s", rec.Code, rec.Body.String())
	}
	r := httptest.NewRequest("POST", "https://postra.test/api/auth/logout", nil)
	r.AddCookie(&http.Cookie{Name: "postra_session", Value: member})
	r.Header.Set("Authorization", "Bearer invalid-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("invalid bearer must not fall back to cookie: %d", rec.Code)
	}
	rec = browserRequest(h, "POST", "/api/auth/logout", member, memberCSRF, "https://postra.test", "")
	if rec.Code != 200 {
		t.Fatalf("logout failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec = browserRequest(h, "GET", "/api/accounts", member, "", "", ""); rec.Code != 401 {
		t.Fatalf("logged-out session still works: %d", rec.Code)
	}
}

func TestBrowserSessionAnonymousAndAuthDisabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		app := browserTestApp(t, enabled)
		h := New(app, "").Handler()
		rec := browserRequest(h, "GET", "/api/auth/session", "", "", "", "")
		want := `"authenticated":true`
		if enabled {
			want = `"authenticated":false`
		}
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) || !strings.Contains(rec.Body.String(), "/app/login") {
			t.Fatalf("anonymous/dev bridge failed: %d %s", rec.Code, rec.Body.String())
		}
		if !enabled {
			if rec = browserRequest(h, "GET", "/api/me", "", "", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role":"admin"`) {
				t.Fatalf("offline dev principal unavailable: %d %s", rec.Code, rec.Body.String())
			}
			if rec = browserRequest(h, "POST", "/api/auth/logout", "", "", "https://evil.test", ""); rec.Code != 403 {
				t.Fatalf("offline mode accepted cross-origin mutation: %d", rec.Code)
			}
		}
	}
}

func TestBrowserSessionOIDCAutoLoginPolicy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		authEnabled   bool
		configured    bool
		defaultAuto   bool
		storedAuto    string
		wantPresent   bool
		wantAutoLogin bool
	}{
		{"configured_off", true, true, true, "false", true, false},
		{"configured_on", true, true, false, "true", true, true},
		{"unconfigured", true, false, true, "true", false, false},
		{"auth_disabled", false, true, true, "true", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := browserTestApp(t, tc.authEnabled)
			app.Cfg.Auth.OIDCAutoLogin = tc.defaultAuto
			settings := map[string]string{application.SettingOIDCAutoLogin: tc.storedAuto}
			if tc.configured {
				settings[application.SettingOIDCIssuer] = "https://keycloak.corp.local/realms/company"
				settings[application.SettingOIDCClientID] = "postra-browser-test"
				settings[application.SettingOIDCRedirectURL] = "https://postra.test/ui/auth/oidc/callback"
			}
			if err := app.Store.UpsertSettings(context.Background(), settings); err != nil {
				t.Fatal(err)
			}
			rec := browserRequest(New(app, "").Handler(), "GET", "/api/auth/session", "", "", "", "")
			var response map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("session metadata unavailable: %d %s (%v)", rec.Code, rec.Body.String(), err)
			}
			got, present := response["oidc_auto_login"]
			if present != tc.wantPresent || (present && got != tc.wantAutoLogin) {
				t.Fatalf("oidc_auto_login=%v present=%v; want %v present=%v", got, present, tc.wantAutoLogin, tc.wantPresent)
			}
			_, hasURL := response["oidc_url"]
			if hasURL != tc.wantPresent {
				t.Fatalf("OIDC endpoint presence must match configured/auth-enabled policy: %v", response)
			}
		})
	}
}

func TestBearerAPIRequestsRemainIndependentOfBrowserCSRF(t *testing.T) {
	h := New(browserTestApp(t, true), "cli-token").Handler()
	r := httptest.NewRequest("POST", "/api/auth/logout", nil)
	r.Header.Set("Authorization", "Bearer cli-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("explicit bearer auth must remain usable by non-browser clients: %d", w.Code)
	}
}

func TestLegacyBrowserCookieUsesSeparateCSRFWithoutExposingAPIToken(t *testing.T) {
	app := browserTestApp(t, false)
	h := New(app, "legacy-private-token").Handler()
	rec := browserRequest(h, "GET", "/api/auth/session", "legacy-private-token", "", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"authenticated":true`) || strings.Contains(rec.Body.String(), "legacy-private-token") {
		t.Fatalf("legacy token-cookie bridge failed: %d %s", rec.Code, rec.Body.String())
	}
	var csrf *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "postra_csrf" {
			csrf = cookie
		}
	}
	if csrf == nil || csrf.Value == "" || csrf.HttpOnly || !csrf.Secure {
		t.Fatalf("legacy bridge must issue only a separate readable secure CSRF cookie: %+v", csrf)
	}
	r := httptest.NewRequest("POST", "https://postra.test/api/auth/logout", nil)
	r.Header.Set("Origin", "https://postra.test")
	r.AddCookie(&http.Cookie{Name: "postra_session", Value: "legacy-private-token"})
	r.AddCookie(csrf)
	r.Header.Set("X-CSRF-Token", csrf.Value)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("legacy valid CSRF logout rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSearchFolderAndImportantFilters(t *testing.T) {
	app := browserTestApp(t, true)
	session, _ := browserIdentity(t, app, "reader", domain.RoleUser)
	for _, important := range []bool{false, true} {
		id := "normal"
		if important {
			id = "important"
		}
		m := &domain.Message{ID: id, UserID: "reader", AccountID: "acc-reader", UIDL: id, RawHash: id, Subject: id, IsImportant: important}
		if err := app.Store.InsertMessage(context.Background(), m, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := app.Store.UpdateMessage(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	h := New(app, "").Handler()
	for _, query := range []string{"folder=important", "is_important=true"} {
		rec := browserRequest(h, "GET", "/api/messages?"+query, session, "", "", "")
		var result domain.SearchResult
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != 200 || len(result.Messages) != 1 || result.Messages[0].ID != "important" {
			t.Fatalf("filter %q was ignored: %d %s (%v)", query, rec.Code, rec.Body.String(), err)
		}
	}
}
