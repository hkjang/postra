package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeIssuer serves just enough OIDC discovery for BeginOIDC to build an
// authorization URL. Its issuer is the server's own URL, as go-oidc requires.
func fakeIssuer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/auth",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSafeReturnTo(t *testing.T) {
	cases := map[string]bool{
		"/ui/":                       true,
		"/ui/messages/msg_1?x=1":     true,
		"":                           false,
		"ui/messages":                false,
		"//evil.example/":            false,
		"/\\evil.example/":           false,
		"https://evil.example/":      false,
		"/ui/\r\nLocation: https://": false,
	}
	for in, want := range cases {
		if got := SafeReturnTo(in); got != want {
			t.Errorf("SafeReturnTo(%q)=%v want %v", in, got, want)
		}
	}
	if got := OIDCReturnToOrDefault("//evil.example/"); got != OIDCDefaultReturnTo {
		t.Fatalf("unsafe return_to was not replaced: %q", got)
	}
}

func TestOIDCLoginRequired(t *testing.T) {
	for _, code := range []string{"login_required", "interaction_required", "consent_required"} {
		if !OIDCLoginRequired(code) {
			t.Errorf("%s should count as the provider's no-session answer", code)
		}
	}
	if OIDCLoginRequired("invalid_request") || OIDCLoginRequired("") {
		t.Error("real provider errors must not be treated as login_required")
	}
}

// TestBeginOIDCSilentOnlyWhenAutoLoginEnabled pins the server-side guard: a
// ?prompt=none start is downgraded to a normal login unless the administrator
// switched auto_login on, so nobody can change the flow from the address bar.
func TestBeginOIDCSilentOnlyWhenAutoLoginEnabled(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	issuer := fakeIssuer(t)
	app.Cfg.Auth.OIDCIssuer = issuer.URL
	app.Cfg.Auth.OIDCClientID = "postra"
	app.Cfg.Auth.OIDCRedirectURL = "http://127.0.0.1:8480/ui/auth/oidc/callback"
	ctx := WithActor(context.Background(), "test")

	// Default install: auto_login is off.
	if app.OIDCAutoLoginEnabled(ctx) {
		t.Fatal("auto_login must default to off")
	}
	authURL, flow, err := app.BeginOIDC(ctx, OIDCStartOptions{Silent: true, ReturnTo: "/ui/messages/m1"})
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, authURL)
	if q.Get("prompt") != "" || flow.Silent {
		t.Fatalf("prompt=none must be dropped while auto_login is off: url=%s flow=%+v", authURL, flow)
	}
	if flow.ReturnTo != "/ui/messages/m1" {
		t.Fatalf("safe return_to was not kept: %+v", flow)
	}

	// Administrator enables it.
	admin, err := app.SetupInitialAdmin(ctx, "admin", "Admin", "admin-password-long")
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := WithPrincipal(ctx, principalFor(admin, "local"))
	if err := app.AdminSaveSettings(adminCtx, map[string]string{SettingOIDCAutoLogin: "true"}, ""); err != nil {
		t.Fatal(err)
	}
	if !app.OIDCAutoLoginEnabled(ctx) {
		t.Fatal("auto_login setting was not applied")
	}
	authURL, flow, err = app.BeginOIDC(ctx, OIDCStartOptions{Silent: true, ReturnTo: "//evil.example/"})
	if err != nil {
		t.Fatal(err)
	}
	q = mustQuery(t, authURL)
	if q.Get("prompt") != "none" || !flow.Silent {
		t.Fatalf("silent attempt was not sent as prompt=none: url=%s flow=%+v", authURL, flow)
	}
	if flow.ReturnTo != OIDCDefaultReturnTo {
		t.Fatalf("open redirect accepted as return_to: %+v", flow)
	}
	// An ordinary click on the SSO button stays interactive even with auto_login on.
	authURL, flow, err = app.BeginOIDC(ctx, OIDCStartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mustQuery(t, authURL).Get("prompt") != "" || flow.Silent {
		t.Fatalf("non-silent start gained prompt=none: %s", authURL)
	}
	// The flag survives the signed round trip so the callback can see it.
	signed, err := app.SignOIDCFlow(OIDCFlow{State: "s", Nonce: "n", CodeVerifier: "v", ExpiresAt: flow.ExpiresAt, Silent: true, ReturnTo: "/ui/x"})
	if err != nil {
		t.Fatal(err)
	}
	back, err := app.VerifyOIDCFlow(signed)
	if err != nil || !back.Silent || back.ReturnTo != "/ui/x" {
		t.Fatalf("silent/return_to lost in signed flow: %+v err=%v", back, err)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestOIDCErrorNoteIsSignedOneShotAndSeparateFromFlows(t *testing.T) {
	app, _, _, _ := newTestApp(t)

	note, err := app.SignOIDCError("Keycloak 로그인이 실패했습니다: invalid_client")
	if err != nil {
		t.Fatal(err)
	}
	if msg, ok := app.VerifyOIDCError(note); !ok || msg != "Keycloak 로그인이 실패했습니다: invalid_client" {
		t.Fatalf("round trip: ok=%v msg=%q", ok, msg)
	}
	// A note is not a login flow and a login flow is not a note, even though
	// both are signed with the same key: neither can be replayed as the other.
	if _, err := app.VerifyOIDCFlow(note); err == nil {
		t.Fatal("error note verified as a flow cookie")
	}
	flow, err := app.SignOIDCFlow(OIDCFlow{State: "st", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := app.VerifyOIDCError(flow); ok {
		t.Fatal("flow cookie verified as an error note")
	}
	// Tampering with the message or the signature is rejected.
	payload, sig, _ := strings.Cut(note, ".")
	if _, ok := app.VerifyOIDCError(payload + ".AAAA"); ok {
		t.Fatal("bad signature accepted")
	}
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"message":"phish","expires_at":9999999999}`))
	if _, ok := app.VerifyOIDCError(forged + "." + sig); ok {
		t.Fatal("forged message accepted")
	}
	// A stale note is refused; an empty message is never shown.
	stale := base64.RawURLEncoding.EncodeToString([]byte(`{"message":"old","expires_at":1}`))
	staleSigned := stale + "." + base64.RawURLEncoding.EncodeToString(app.oidcMAC(oidcErrorDomain, []byte(`{"message":"old","expires_at":1}`)))
	if _, ok := app.VerifyOIDCError(staleSigned); ok {
		t.Fatal("stale note accepted")
	}
	if _, ok := app.VerifyOIDCError(""); ok {
		t.Fatal("empty note accepted")
	}
	// A provider that pastes its whole body into the reason cannot overflow
	// the cookie: the note is cut on a rune boundary, never mid-character.
	long, err := app.SignOIDCError(strings.Repeat("가", 2000))
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := app.VerifyOIDCError(long)
	if !ok || len(msg) > oidcErrorNoteMaxBytes+len("…") || !utf8.ValidString(msg) || !strings.HasSuffix(msg, "…") {
		t.Fatalf("long note: ok=%v len=%d valid=%v", ok, len(msg), utf8.ValidString(msg))
	}
}
