package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"postra/internal/application"
)

func authJSON(h http.Handler, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://postra.test"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Origin", "https://postra.test")
	r.Header.Set("Content-Type", "application/json")
	for _, cookie := range cookies {
		r.AddCookie(cookie)
		if cookie.Name == "postra_csrf" {
			r.Header.Set("X-CSRF-Token", cookie.Value)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestStandaloneAuthSetupLoginLogoutAndCSRF(t *testing.T) {
	h := New(browserTestApp(t, true), "").Handler()
	setupBody := `{"login_id":"admin-test","display_name":"테스트 관리자","password":"fixture-password-2026"}`
	r := httptest.NewRequest("POST", "https://postra.test/auth/setup", strings.NewReader(setupBody))
	r.RemoteAddr = "198.51.100.10:12345"
	r.Header.Set("Origin", "https://postra.test")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("remote bootstrap unexpectedly allowed: %d", w.Code)
	}
	w = authJSON(h, "/auth/setup", setupBody)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), "/app/admin/settings") {
		t.Fatalf("setup failed: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if !c.Secure || c.Path != "/" || (c.Name == "postra_session" && !c.HttpOnly) {
			t.Fatalf("insecure auth cookie metadata: %s", c.Name)
		}
		if strings.Contains(w.Body.String(), c.Value) {
			t.Fatal("auth credential appeared in JSON")
		}
	}
	w = authJSON(h, "/auth/setup", setupBody)
	if w.Code != http.StatusConflict {
		t.Fatalf("bootstrap was not one-time: %d", w.Code)
	}
	w = authJSON(h, "/auth/logout", "", cookies...)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/app/login?sso=signed_out") {
		t.Fatalf("logout failed: %d", w.Code)
	}
	for _, c := range cookies {
		if c.Name == "postra_session" {
			if response := browserRequest(h, "GET", "/auth/session", c.Value, "", "", ""); strings.Contains(response.Body.String(), `"authenticated":true`) {
				t.Fatal("logged out session remains valid")
			}
		}
	}
	w = authJSON(h, "/auth/login", `{"login_id":"admin-test","password":"wrong-test-password"}`)
	if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "wrong-test-password") {
		t.Fatalf("invalid credentials were not safely rejected: %d", w.Code)
	}
	w = authJSON(h, "/auth/login", `{"login_id":"admin-test","password":"fixture-password-2026","return_to":"/app/messages/m1?body=true"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/app/messages/m1?body=true") {
		t.Fatalf("login/deep return failed: %d %s", w.Code, w.Body.String())
	}
	for _, origin := range []string{"", "https://evil.test", "https://postra.test:444"} {
		r := httptest.NewRequest("POST", "https://postra.test/auth/login", strings.NewReader(`{}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("login CSRF origin %q accepted: %d", origin, w.Code)
		}
	}
	w = authJSON(h, "/auth/login", `{} {}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("multiple JSON bodies accepted: %d", w.Code)
	}
}

func TestStandaloneTokenLoginAndSafeReturn(t *testing.T) {
	h := New(browserTestApp(t, false), "browser-fixture-token").Handler()
	w := authJSON(h, "/auth/login", `{"token":"browser-fixture-token","return_to":"//evil.test"}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "browser-fixture-token") || !strings.Contains(w.Body.String(), `"redirect_url":"/app/"`) {
		t.Fatalf("token login response unsafe: %d", w.Code)
	}
	if len(w.Result().Cookies()) != 2 {
		t.Fatal("token mode needs separate session and CSRF cookies")
	}
	for _, path := range []string{"https://evil.test", "//evil.test", "/app/../outside", "/app/%2e%2e/outside", "/ui/", "/app/login", "/app/setup"} {
		if browserReturnTo(path) != "/app/" {
			t.Fatalf("unsafe/recursive redirect accepted: %q", path)
		}
	}
}

func TestOIDCCallbackErrorNoteDoesNotTrustProviderText(t *testing.T) {
	const secret = "private-idp-client-secret-2983"
	const code = "private-authorization-code-8127"
	const jwt = "private-id-token-value-3815"
	const unknown = "private-unrecognized-oauth-error-3729"
	for _, providerError := range []string{"invalid_client", unknown} {
		t.Run(providerError, func(t *testing.T) {
			app := browserTestApp(t, true)
			h := New(app, "").Handler()
			flow := application.OIDCFlow{State: "fixture-state", ReturnTo: "/app/messages/fixture-message?body=true", ExpiresAt: time.Now().Add(time.Minute).Unix()}
			signed, err := app.SignOIDCFlow(flow)
			if err != nil {
				t.Fatal(err)
			}
			query := url.Values{"state": {flow.State}, "error": {providerError}, "error_description": {secret + " " + code + " " + jwt}, "error_uri": {"https://idp.invalid/" + jwt}}
			r := httptest.NewRequest("GET", "https://postra.test/auth/oidc/callback?"+query.Encode(), nil)
			r.AddCookie(&http.Cookie{Name: "postra_oidc_flow", Value: signed})
			callback := httptest.NewRecorder()
			h.ServeHTTP(callback, r)
			location, err := url.Parse(callback.Header().Get("Location"))
			if err != nil || callback.Code != http.StatusFound || location.Query().Get("sso") != "error" || location.Query().Get("return_to") != flow.ReturnTo {
				t.Fatal("sanitized error lost retry marker or deep return")
			}
			var note *http.Cookie
			for _, cookie := range callback.Result().Cookies() {
				if cookie.Name == browserOIDCErrorCookie {
					note = cookie
				}
			}
			if note == nil || !note.HttpOnly || note.Path != "/auth/session" || note.MaxAge != int(application.OIDCErrorNoteTTL.Seconds()) {
				t.Fatal("missing one-shot, scoped OIDC error note")
			}
			message, valid := app.VerifyOIDCError(note.Value)
			if !valid || providerError == "invalid_client" && !strings.Contains(message, "invalid_client") {
				t.Fatal("safe actionable OAuth code was not preserved")
			}
			r = httptest.NewRequest("GET", "https://postra.test/auth/session?sso=error", nil)
			r.AddCookie(note)
			session := httptest.NewRecorder()
			h.ServeHTTP(session, r)
			for _, output := range []string{message, session.Body.String(), callback.Header().Get("Location"), callback.Body.String()} {
				for _, private := range []string{secret, code, jwt, unknown} {
					if strings.Contains(output, private) {
						t.Fatal("provider error credentials leaked into the signed note, session JSON or redirect")
					}
				}
			}
			consumed := false
			for _, cookie := range session.Result().Cookies() {
				if cookie.Name == browserOIDCErrorCookie && cookie.MaxAge < 0 {
					consumed = true
				}
			}
			if !consumed {
				t.Fatal("safe error note was not consumed after display")
			}
		})
	}
}

type browserOIDCProvider struct {
	server   *httptest.Server
	nonce    atomic.Value
	redirect chan string
}

func newBrowserOIDCProvider(t *testing.T) *browserOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &browserOIDCProvider{redirect: make(chan string, 1)}
	p.nonce.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		p.nonce.Store(r.URL.Query().Get("nonce"))
		target, err := url.Parse(r.URL.Query().Get("redirect_uri"))
		if err != nil {
			http.Error(w, "invalid fixture return", 400)
			return
		}
		query := target.Query()
		query.Set("state", r.URL.Query().Get("state"))
		query.Set("code", "fixture-code")
		target.RawQuery = query.Encode()
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
	})
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "browser-key", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		p.redirect <- r.FormValue("redirect_uri")
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "browser-key"))
		if err != nil {
			http.Error(w, "fixture signer unavailable", 500)
			return
		}
		claims, _ := json.Marshal(map[string]any{"iss": p.server.URL, "aud": "browser-client", "sub": "browser-subject", "nonce": p.nonce.Load(), "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "email": "oidc-browser@corp.local", "email_verified": true, "preferred_username": "oidc-browser", "name": "SSO 사용자"})
		signed, err := signer.Sign(claims)
		if err != nil {
			http.Error(w, "fixture signing failed", 500)
			return
		}
		idToken, err := signed.CompactSerialize()
		if err != nil {
			http.Error(w, "fixture token failed", 500)
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "expires_in": 60, "id_token": idToken})
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func TestStandaloneOIDCFlowErrorAndLegacyRedirectURI(t *testing.T) {
	app := browserTestApp(t, true)
	provider := newBrowserOIDCProvider(t)
	legacyRedirect := "https://postra.test/ui/auth/oidc/callback"
	if err := app.Store.UpsertSettings(context.Background(), map[string]string{application.SettingOIDCIssuer: provider.server.URL, application.SettingOIDCClientID: "browser-client", application.SettingOIDCRedirectURL: legacyRedirect, application.SettingOIDCAutoLogin: "true", application.SettingOIDCAutoProvision: "true"}); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()
	start := browserRequest(h, "GET", "/auth/oidc/start?prompt=none&return_to=%2Fapp%2Fmessages%2Fm1", "", "", "", "")
	if start.Code != http.StatusSeeOther {
		t.Fatalf("OIDC start failed: %d", start.Code)
	}
	authorize, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if authorize.Query().Get("prompt") != "none" || authorize.Query().Get("redirect_uri") != legacyRedirect {
		t.Fatal("silent policy or configured legacy redirect URI changed")
	}
	var cookie *http.Cookie
	for _, c := range start.Result().Cookies() {
		if c.Name == "postra_oidc_flow" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Path != "/auth" || !cookie.HttpOnly || !cookie.Secure {
		t.Fatal("OIDC flow cookie is not scoped to /auth")
	}
	flow, err := app.VerifyOIDCFlow(cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	callback := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://postra.test/auth/oidc/callback?"+query, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	refused := callback(url.Values{"state": {flow.State}, "error": {"login_required"}}.Encode())
	if !strings.HasPrefix(refused.Header().Get("Location"), "/app/login?") || !strings.Contains(refused.Header().Get("Location"), "sso=none") {
		t.Fatal("silent refusal did not terminate at SPA login")
	}
	failed := callback(url.Values{"state": {flow.State}, "error": {"invalid_scope"}, "error_description": {"groups scope unavailable"}}.Encode())
	if strings.Contains(failed.Header().Get("Location"), flow.State) || !strings.Contains(failed.Header().Get("Location"), "sso=error") {
		t.Fatal("callback code/state leaked or error retry marker missing")
	}
	var note *http.Cookie
	for _, c := range failed.Result().Cookies() {
		if c.Name == browserOIDCErrorCookie {
			note = c
		}
	}
	if note == nil || note.Path != "/auth/session" || !note.HttpOnly {
		t.Fatal("missing scoped signed error note")
	}
	r := httptest.NewRequest("GET", "https://postra.test/auth/session?sso=error", nil)
	r.AddCookie(note)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	// Keep the actionable scope diagnosis, never the IdP's arbitrary text.
	if !strings.Contains(w.Body.String(), "invalid_scope") || !strings.Contains(w.Body.String(), "groups") || strings.Contains(w.Body.String(), "groups scope unavailable") {
		t.Fatal("signed error note did not show sanitized scope guidance")
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == browserOIDCErrorCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("error note was not consumed")
	}
	note.Value = "forged"
	r = httptest.NewRequest("GET", "https://postra.test/auth/session?sso=error", nil)
	r.AddCookie(note)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "invalid_scope") || strings.Contains(w.Body.String(), "forged") {
		t.Fatal("forged note was trusted")
	}
	provider.nonce.Store(flow.Nonce)
	success := callback(url.Values{"state": {flow.State}, "code": {"fixture-code"}}.Encode())
	if success.Code != http.StatusSeeOther || success.Header().Get("Location") != "/app/messages/m1" {
		t.Fatalf("OIDC callback failed: %d %s", success.Code, success.Header().Get("Location"))
	}
	if got := <-provider.redirect; got != legacyRedirect {
		t.Fatal("token exchange did not preserve configured redirect URI")
	}
	var session string
	for _, c := range success.Result().Cookies() {
		if c.Name == "postra_session" {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("OIDC callback did not establish the shared browser session")
	}
	if _, principal, err := app.AuthenticateSession(context.Background(), session); err != nil || principal.AuthMethod != "oidc" {
		t.Fatalf("OIDC session invalid: %v", err)
	}
}
