package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
)

const silentTrigger = "/ui/auth/oidc/start?prompt=none"

// oidcTestApp is an auth-enabled app whose OIDC issuer is a fake discovery
// server, so the start handler can build real authorization URLs offline.
func oidcTestApp(t *testing.T) *application.App {
	t.Helper()
	app, _ := newTestApp(t)
	app.Cfg.Auth.Enabled = true
	if _, err := app.SetupInitialAdmin(context.Background(), "admin", "Administrator", "a-secure-password"); err != nil {
		t.Fatal(err)
	}
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/auth",
			"token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/jwks",
		})
	}))
	t.Cleanup(issuer.Close)
	app.Cfg.Auth.OIDCIssuer = issuer.URL
	app.Cfg.Auth.OIDCClientID = "postra"
	app.Cfg.Auth.OIDCRedirectURL = "http://127.0.0.1:8480/ui/auth/oidc/callback"
	return app
}

func flowCookie(t *testing.T, app *application.App, flow application.OIDCFlow) *http.Cookie {
	t.Helper()
	signed, err := app.SignOIDCFlow(flow)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "postra_oidc_flow", Value: signed}
}

func TestGateCarriesDeepLinkToLogin(t *testing.T) {
	app := oidcTestApp(t)
	h := New(app, "").Handler()

	rec := do(t, h, http.MethodGet, "/ui/messages/msg_1?tab=raw", nil, nil)
	want := "/ui/login?return_to=" + url.QueryEscape("/ui/messages/msg_1?tab=raw")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
		t.Fatalf("deep link: code=%d location=%q want %q", rec.Code, rec.Header().Get("Location"), want)
	}
	// The default landing page carries nothing.
	rec = do(t, h, http.MethodGet, "/ui/", nil, nil)
	if rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("root: location=%q", rec.Header().Get("Location"))
	}
	// Manual login honours the carried deep link and refuses off-site targets.
	rec = do(t, h, http.MethodPost, "/ui/login", url.Values{
		"login_id": {"admin"}, "password": {"a-secure-password"}, "return_to": {"/ui/messages/msg_1"},
	}, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/messages/msg_1" {
		t.Fatalf("login with return_to: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(t, h, http.MethodPost, "/ui/login", url.Values{
		"login_id": {"admin"}, "password": {"a-secure-password"}, "return_to": {"//evil.example/"},
	}, nil)
	if rec.Header().Get("Location") != "./" {
		t.Fatalf("open redirect via return_to: location=%q", rec.Header().Get("Location"))
	}
}

func TestSilentSSOAttemptOnlyWhenAutoLoginOn(t *testing.T) {
	app := oidcTestApp(t)
	h := New(app, "").Handler()

	// Default install: the login page renders no silent attempt at all, and a
	// hand-written ?prompt=none start is downgraded to an ordinary login.
	rec := do(t, h, http.MethodGet, "/ui/login", nil, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), silentTrigger) {
		t.Fatalf("auto_login off: code=%d trigger present=%v", rec.Code, strings.Contains(rec.Body.String(), silentTrigger))
	}
	rec = do(t, h, http.MethodGet, silentTrigger+"&return_to=%2Fui%2Fmessages%2Fm1", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("start: code=%d body=%s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("prompt") != "" {
		t.Fatalf("prompt=none forwarded although auto_login is off: %s", loc)
	}

	// Administrator turns it on.
	ctx := application.WithActor(context.Background(), "test")
	admin, _, err := app.Store.GetUserByLogin(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := application.WithPrincipal(ctx, domain.Principal{UserID: admin.ID, Role: admin.Role, AuthMethod: "local"})
	if err := app.AdminSaveSettings(adminCtx, map[string]string{application.SettingOIDCAutoLogin: "true"}, ""); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, http.MethodGet, "/ui/login?return_to=%2Fui%2Fmessages%2Fm1", nil, nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, silentTrigger) {
		t.Fatalf("auto_login on: silent attempt missing from login page (code=%d)", rec.Code)
	}
	if !strings.Contains(body, `name="return_to" value="/ui/messages/m1"`) {
		t.Fatalf("deep link not carried into the login form:\n%s", body)
	}
	rec = do(t, h, http.MethodGet, silentTrigger+"&return_to=%2Fui%2Fmessages%2Fm1", nil, nil)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || loc.Query().Get("prompt") != "none" {
		t.Fatalf("silent start: code=%d location=%s", rec.Code, loc)
	}
	var flow application.OIDCFlow
	for _, c := range rec.Result().Cookies() {
		if c.Name == "postra_oidc_flow" {
			flow, err = app.VerifyOIDCFlow(c.Value)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if !flow.Silent || flow.ReturnTo != "/ui/messages/m1" {
		t.Fatalf("flow cookie does not record the silent attempt: %+v", flow)
	}

	// Refusal and logout markers keep the page from trying again, and an
	// error render never carries the trigger either.
	for _, target := range []string{"/ui/login?sso=none", "/ui/login?sso=signed_out", "/ui/login?sso=none&return_to=%2Fui%2Fx"} {
		rec = do(t, h, http.MethodGet, target, nil, nil)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), silentTrigger) {
			t.Fatalf("%s: code=%d must not retry silently", target, rec.Code)
		}
	}
	rec = do(t, h, http.MethodGet, "/ui/login?sso=signed_out", nil, nil)
	if !strings.Contains(rec.Body.String(), "postra.sso.signedOut") {
		t.Fatal("signed-out landing does not record the suppression marker")
	}
	rec = do(t, h, http.MethodPost, "/ui/login", url.Values{"login_id": {"admin"}, "password": {"wrong"}}, nil)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), silentTrigger) {
		t.Fatalf("error render: code=%d must not retry silently", rec.Code)
	}
}

func TestSilentSSOCallbackRefusalLandsOnLoginWithMarker(t *testing.T) {
	app := oidcTestApp(t)
	h := New(app, "").Handler()
	expires := time.Now().Add(time.Minute).Unix()

	// login_required for a silent flow is the provider's normal "no session"
	// answer: go to the login page with the marker and the deep link.
	silent := flowCookie(t, app, application.OIDCFlow{State: "st", Nonce: "n", CodeVerifier: "v",
		ExpiresAt: expires, Silent: true, ReturnTo: "/ui/messages/m1"})
	rec := do(t, h, http.MethodGet, "/ui/auth/oidc/callback?error=login_required&state=st", nil, silent)
	want := "/ui/login?sso=none&return_to=" + url.QueryEscape("/ui/messages/m1")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
		t.Fatalf("silent refusal: code=%d location=%q want %q", rec.Code, rec.Header().Get("Location"), want)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "postra_oidc_flow" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("flow cookie survived the refusal")
	}
	// The plain landing page carries no return_to noise.
	silentRoot := flowCookie(t, app, application.OIDCFlow{State: "st", Nonce: "n", CodeVerifier: "v",
		ExpiresAt: expires, Silent: true, ReturnTo: "/ui/"})
	rec = do(t, h, http.MethodGet, "/ui/auth/oidc/callback?error=interaction_required&state=st", nil, silentRoot)
	if rec.Header().Get("Location") != "/ui/login?sso=none" {
		t.Fatalf("silent refusal to root: location=%q", rec.Header().Get("Location"))
	}

	// A real provider error on a silent flow, or login_required on an
	// interactive flow, is a failure: it lands on sso=error, never sso=none.
	rec = do(t, h, http.MethodGet, "/ui/auth/oidc/callback?error=invalid_request&error_description=bad+scope&state=st", nil, silent)
	want = "/ui/login?sso=error&return_to=" + url.QueryEscape("/ui/messages/m1")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
		t.Fatalf("real error on silent flow: code=%d location=%q want %q", rec.Code, rec.Header().Get("Location"), want)
	}
	if msg := errorNote(t, app, rec); !strings.Contains(msg, "bad scope") {
		t.Fatalf("error_description lost on the way to the login page: %q", msg)
	}
	interactive := flowCookie(t, app, application.OIDCFlow{State: "st", Nonce: "n", CodeVerifier: "v", ExpiresAt: expires})
	rec = do(t, h, http.MethodGet, "/ui/auth/oidc/callback?error=login_required&state=st", nil, interactive)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login?sso=error" {
		t.Fatalf("login_required on interactive flow: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if msg := errorNote(t, app, rec); !strings.Contains(msg, "login_required") {
		t.Fatalf("provider error code lost: %q", msg)
	}
	// A state mismatch cannot turn a stranger's error into a silent redirect.
	rec = do(t, h, http.MethodGet, "/ui/auth/oidc/callback?error=login_required&state=other", nil, silent)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login?sso=error" {
		t.Fatalf("state mismatch treated as silent refusal: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// errorNote returns the verified reason carried by the signed error cookie set
// on a callback failure response.
func errorNote(t *testing.T, app *application.App, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcErrorCookie && c.MaxAge > 0 {
			if !c.HttpOnly || c.Path != "/ui/login" {
				t.Fatalf("error note cookie must be HttpOnly and scoped to the login page: %+v", c)
			}
			msg, ok := app.VerifyOIDCError(c.Value)
			if !ok {
				t.Fatal("error note does not verify")
			}
			return msg
		}
	}
	t.Fatal("callback failure left no error note")
	return ""
}

func TestOIDCFailureLandsOnLoginWithErrorMarker(t *testing.T) {
	app := oidcTestApp(t)
	h := New(app, "").Handler()
	ctx := application.WithActor(context.Background(), "test")
	admin, _, err := app.Store.GetUserByLogin(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := application.WithPrincipal(ctx, domain.Principal{UserID: admin.ID, Role: admin.Role, AuthMethod: "local"})
	if err := app.AdminSaveSettings(adminCtx, map[string]string{application.SettingOIDCAutoLogin: "true"}, ""); err != nil {
		t.Fatal(err)
	}

	// A callback without a usable flow (no cookie, or a code for someone
	// else's state) never renders on the callback address: the browser is
	// sent to the login page and the flow cookie is dropped so a refresh
	// cannot resubmit the code.
	rec := do(t, h, http.MethodGet, "/ui/auth/oidc/callback?code=abc&state=st", nil, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login?sso=error" {
		t.Fatalf("no flow: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	errorNote(t, app, rec)
	flowCleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "postra_oidc_flow" && c.MaxAge < 0 {
			flowCleared = true
		}
	}
	if !flowCleared {
		t.Fatal("flow cookie survived the failure")
	}

	// The login page shows the reason once (200, not the callback's status),
	// clears the note and — auto_login being on — still never retries silently.
	signed, err := app.SignOIDCError("Keycloak 로그인이 실패했습니다: invalid_client — bad secret")
	if err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, http.MethodGet, "/ui/login?sso=error&return_to=%2Fui%2Fmessages%2Fm1", nil, &http.Cookie{Name: oidcErrorCookie, Value: signed})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "bad secret") {
		t.Fatalf("error landing: code=%d reason shown=%v", rec.Code, strings.Contains(body, "bad secret"))
	}
	if strings.Contains(body, silentTrigger) {
		t.Fatal("error landing must not retry silently")
	}
	if !strings.Contains(body, `name="return_to" value="/ui/messages/m1"`) {
		t.Fatal("deep link not carried into the login form after a failure")
	}
	noteCleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcErrorCookie && c.MaxAge < 0 {
			noteCleared = true
		}
	}
	if !noteCleared {
		t.Fatal("error note survived being shown")
	}

	// A forged or stale note is not reflected; a bare sso=error link gets the
	// generic message and still no silent attempt.
	forged := signed[:strings.LastIndex(signed, ".")+1] + "AAAA"
	rec = do(t, h, http.MethodGet, "/ui/login?sso=error", nil, &http.Cookie{Name: oidcErrorCookie, Value: forged})
	if strings.Contains(rec.Body.String(), "bad secret") {
		t.Fatal("forged error note was reflected")
	}
	rec = do(t, h, http.MethodGet, "/ui/login?sso=error", nil, nil)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "SSO 로그인이 실패했습니다") || strings.Contains(body, silentTrigger) {
		t.Fatalf("bare sso=error: code=%d generic shown=%v trigger=%v", rec.Code,
			strings.Contains(body, "SSO 로그인이 실패했습니다"), strings.Contains(body, silentTrigger))
	}
	// The ordinary login page is unaffected: no error, silent attempt allowed.
	rec = do(t, h, http.MethodGet, "/ui/login", nil, nil)
	if !strings.Contains(rec.Body.String(), silentTrigger) || strings.Contains(rec.Body.String(), "SSO 로그인이 실패했습니다") {
		t.Fatal("plain login page changed")
	}
}

func TestLogoutLandsWithSignedOutMarker(t *testing.T) {
	app := oidcTestApp(t)
	h := New(app, "").Handler()
	rec := do(t, h, http.MethodPost, "/ui/login", url.Values{"login_id": {"admin"}, "password": {"a-secure-password"}}, nil)
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("login did not set a session cookie")
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/logout", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login?sso=signed_out" {
		t.Fatalf("logout: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}
