package application

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"postra/internal/domain"
)

type oauthTestIssuer struct {
	srv         *httptest.Server
	mu          sync.RWMutex
	key         *rsa.PrivateKey
	kid         string
	discoveries atomic.Int32
	keyRequests atomic.Int32
	unavailable atomic.Bool
}

func newOAuthTestIssuer(t *testing.T) *oauthTestIssuer {
	t.Helper()
	f := &oauthTestIssuer{}
	f.rotate(t)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.unavailable.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			f.discoveries.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/jwks", "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/jwks":
			f.keyRequests.Add(1)
			f.mu.RLock()
			defer f.mu.RUnlock()
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: f.kid, Algorithm: "RS256", Use: "sig"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}
func (f *oauthTestIssuer) rotate(t *testing.T) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key = key
	f.kid = fmt.Sprintf("key-%d", time.Now().UnixNano())
}
func (f *oauthTestIssuer) token(t *testing.T, override map[string]any) string {
	t.Helper()
	claims := map[string]any{"iss": f.srv.URL, "sub": "subject-owner", "aud": "https://postra.test/mcp", "azp": "desktop", "typ": "Bearer", "scope": "openid email mail.read mail.search mail.send admin.write", "exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix()}
	for k, v := range override {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", f.kid).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func oauthTestApp(t *testing.T) (*App, *oauthTestIssuer) {
	t.Helper()
	app, _, _, _ := newTestApp(t)
	issuer := newOAuthTestIssuer(t)
	values := map[string]string{SettingOIDCIssuer: issuer.srv.URL, SettingOIDCClientID: "postra-web", SettingOIDCRedirectURL: "https://postra.test/auth/oidc/callback", "mcp.oauth.enabled": "true", "mcp.oauth.resource_url": "https://postra.test/mcp", "mcp.oauth.allowed_client_ids": "desktop,desktop-other", "mcp.oauth.allowed_scopes": "mail.read,mail.search,mail.send"}
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: values}); err != nil {
		t.Fatal(err)
	}
	u := &domain.User{ID: "oauth-owner", LoginID: "oauth-owner", Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "oidc", OIDCIssuer: issuer.srv.URL, OIDCSubject: "subject-owner", Email: "owner@example.test"}
	if err := app.Store.CreateUser(context.Background(), u, ""); err != nil {
		t.Fatal(err)
	}
	return app, issuer
}

func TestMCPOAuthAccessTokenValidation(t *testing.T) {
	app, issuer := oauthTestApp(t)
	ctx := context.Background()
	p, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, nil))
	if err != nil || p.UserID != "oauth-owner" || !p.IsMCPScoped() || p.OAuthClientID != "desktop" || p.OAuthIssuer != issuer.srv.URL || p.OAuthExpiresAt.IsZero() || p.Role != domain.RoleUser {
		t.Fatalf("principal = %+v, err %v", p, err)
	}
	if !slices.Equal(p.MCPScopes, []string{"mail.read", "mail.search", "mail.send"}) {
		t.Fatalf("unexpected permissions %v", p.MCPScopes)
	}
	cases := map[string]map[string]any{
		"id-token": {"typ": "ID"}, "no-type": {"typ": nil}, "dpop": {"typ": "DPoP"}, "confirmation": {"cnf": map[string]any{"jkt": "bound-key"}}, "null-confirmation": {"cnf": json.RawMessage("null")},
		"wrong-issuer": {"iss": "https://wrong.test"}, "wrong-aud": {"aud": "postra-web"}, "missing-aud": {"aud": nil}, "client-not-audience": {"aud": "desktop"}, "wrong-client": {"azp": "unregistered"}, "web-client": {"azp": "postra-web"}, "missing-client": {"azp": nil},
		"expired": {"exp": time.Now().Add(-time.Second).Unix()}, "missing-exp": {"exp": nil}, "future-nbf": {"nbf": time.Now().Add(30 * time.Second).Unix()}, "no-sub": {"sub": nil}, "blank-sub": {"sub": " "}, "unlinked-sub": {"sub": "unlinked-subject", "email": "owner@example.test", "email_verified": true},
	}
	for name, claims := range cases {
		t.Run(name, func(t *testing.T) {
			if p, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, claims)); err == nil || p.UserID != "" {
				t.Fatal("invalid credential accepted")
			}
		})
	}
	if _, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, map[string]any{"aud": []string{"account", "https://postra.test/mcp"}})); err != nil {
		t.Fatal("array audience rejected", err)
	}
	for _, scope := range []string{"", "openid mail.readall admin.write", "mail.unknown"} {
		p, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, map[string]any{"scope": scope}))
		if err != nil {
			t.Fatal(err)
		}
		if len(p.MCPScopes) != 0 || CheckMCPScopes(WithPrincipal(ctx, p), "mail.read") == nil {
			t.Fatal("scope escalation", p.MCPScopes)
		}
	}
	other := newOAuthTestIssuer(t)
	if _, err := app.AuthenticateMCPOAuthToken(ctx, other.token(t, map[string]any{"iss": issuer.srv.URL})); err == nil {
		t.Fatal("forged signature accepted")
	}
	if issuer.discoveries.Load() != 1 {
		t.Fatalf("discovery not cached: %d", issuer.discoveries.Load())
	}
}

func TestMCPOAuthLiveRestrictionsAndNoCredentialMinting(t *testing.T) {
	app, issuer := oauthTestApp(t)
	ctx := context.Background()
	token := issuer.token(t, nil)
	p, err := app.AuthenticateMCPOAuthToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.CreateMCPKeyWithScopes(WithPrincipal(ctx, p), "escalate", []string{"admin.write"}); err == nil {
		t.Fatal("OAuth minted an API credential")
	}
	for _, values := range []map[string]string{{"mcp.oauth.allowed_scopes": "mail.read"}, {"mcp.permissions.read": "false"}} {
		if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: values}); err != nil {
			t.Fatal(err)
		}
		p, err = app.AuthenticateMCPOAuthToken(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(p.MCPScopes, "mail.send") || values["mcp.permissions.read"] == "false" && slices.Contains(p.MCPScopes, "mail.read") {
			t.Fatal("live restrictions ignored")
		}
	}
	u, err := app.Store.GetUserByOIDC(ctx, issuer.srv.URL, "subject-owner")
	if err != nil {
		t.Fatal(err)
	}
	u.Status = domain.UserDisabled
	if err := app.Store.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := app.AuthenticateMCPOAuthToken(ctx, token); err == nil {
		t.Fatal("disabled user accepted")
	}
	u.Status = domain.UserActive
	if err := app.Store.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	for _, values := range []map[string]string{{"mcp.oauth.allowed_client_ids": "desktop-other"}, {"mcp.oauth.enabled": "false"}} {
		if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: values}); err != nil {
			t.Fatal(err)
		}
		if _, err := app.AuthenticateMCPOAuthToken(ctx, token); err == nil {
			t.Fatal("revoked client accepted")
		}
	}
}

func TestMCPOAuthCannotEscapeThroughLegacyREST(t *testing.T) {
	app, issuer := oauthTestApp(t)
	for _, claims := range []map[string]any{nil, {"aud": []string{"https://postra.test/mcp", "postra-web"}}, {"aud": "postra-web", "azp": "desktop", "scope": "openid"}, {"aud": "postra-web", "azp": "postra-web"}, {"aud": "postra-web", "azp": "postra-web", "scope": "openid", "typ": "ID"}} {
		if _, err := app.AuthenticateOIDCAccessToken(context.Background(), issuer.token(t, claims)); err == nil {
			t.Fatal("MCP or ID token acquired unrestricted REST authority")
		}
	}
	if _, err := app.AuthenticateOIDCAccessToken(context.Background(), issuer.token(t, map[string]any{"aud": "postra-web", "azp": "postra-web", "scope": "openid email"})); err != nil {
		t.Fatal("dedicated legacy REST credential regressed", err)
	}
}

func TestMCPOAuthJWKSRotationAndOutage(t *testing.T) {
	app, issuer := oauthTestApp(t)
	if _, err := app.AuthenticateMCPOAuthToken(context.Background(), issuer.token(t, nil)); err != nil {
		t.Fatal(err)
	}
	issuer.rotate(t)
	if _, err := app.AuthenticateMCPOAuthToken(context.Background(), issuer.token(t, nil)); err != nil {
		t.Fatal("new signing key not fetched", err)
	}
	if issuer.keyRequests.Load() != 2 {
		t.Fatalf("unexpected JWKS requests: %d", issuer.keyRequests.Load())
	}
	issuer.rotate(t)
	issuer.unavailable.Store(true)
	if _, err := app.AuthenticateMCPOAuthToken(context.Background(), issuer.token(t, nil)); err == nil {
		t.Fatal("unknown signing key accepted during outage")
	}
}

func TestMCPOAuthSettingsAndConnectionInfo(t *testing.T) {
	app, issuer := oauthTestApp(t)
	for _, values := range []map[string]string{{"mcp.oauth.resource_url": "http://postra.test/mcp"}, {"mcp.oauth.resource_url": "https://user:password@postra.test/mcp"}, {"mcp.oauth.resource_url": "https://postra.test/mcp?secret=x"}, {"mcp.oauth.resource_url": "https://postra.test/mcp?"}, {"mcp.oauth.resource_url": "https://postra.test/mcp#"}, {"mcp.oauth.resource_url": "https://postra.test/other"}, {"mcp.oauth.allowed_client_ids": "postra-web"}, {"mcp.oauth.allowed_client_ids": ""}, {"mcp.oauth.allowed_client_ids": "bad\"id"}, {"mcp.oauth.allowed_scopes": "mail.*"}, {SettingOIDCIssuer: "http://keycloak.test/realms/corp"}, {"mcp.endpoint": "/.well-known/oauth-protected-resource"}} {
		if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: values}); err == nil {
			t.Fatalf("invalid configuration accepted: %v", values)
		}
	}
	o := app.MCPOAuthConnection()
	if !o.OAuth.Configured || o.OAuth.MetadataURL != "https://postra.test/.well-known/oauth-protected-resource/mcp" || o.OAuth.Issuer != issuer.srv.URL {
		t.Fatalf("invalid connection info %+v", o)
	}
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: map[string]string{"mcp.endpoint": "/agent", "mcp.oauth.resource_url": "https://postra.test/agent"}}); err != nil {
		t.Fatal(err)
	}
	o = app.MCPOAuthConnection()
	if !o.PendingRestart || o.ActiveEndpoint != "/mcp" || o.ConfiguredEndpoint != "/agent" || o.OAuth.Configured {
		t.Fatal("pending endpoint advertised as active", o)
	}
	for _, raw := range []string{"https://host/mcp", "http://localhost:8080/mcp", "http://127.0.0.1/mcp", "http://[::1]/mcp"} {
		if !validOAuthURL(raw) {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{"http://localhost.evil/mcp", "https://host/../mcp", "https://host//mcp", "https://host/%6dcp", "javascript:alert(1)", "https://host/mcp\n"} {
		if validOAuthURL(raw) {
			t.Fatal(raw)
		}
	}
	serialized, _ := json.Marshal(o)
	if strings.Contains(string(serialized), "password") {
		t.Fatal("credential in connection metadata")
	}
}

func TestMCPOAuthMetadataResponseBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.(http.Flusher).Flush() // no Content-Length; bound the actual body too
		_, _ = io.WriteString(w, `{}`+strings.Repeat(" ", 1<<20))
	}))
	t.Cleanup(server.Close)
	client := &http.Client{Transport: &oauthTransport{base: http.DefaultTransport, issuer: server.URL}, Timeout: 2 * time.Second}
	if response, err := client.Get(server.URL + "/.well-known/openid-configuration"); err == nil {
		response.Body.Close()
		t.Fatal("oversized chunked metadata accepted")
	}
}

func TestMCPOAuthConnectionRedactsInvalidImportedURLs(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	app.applyRuntimeSettings(map[string]string{SettingOIDCIssuer: "https://user:private-password@sso.test/realms/corp", "mcp.oauth.resource_url": "https://postra.test/mcp?api_key=private-key"})
	info := app.MCPOAuthConnection()
	raw, err := json.Marshal(info)
	if err != nil || info.OAuth.Configured || info.OAuth.Issuer != "" || info.OAuth.ResourceURL != "" || strings.Contains(string(raw), "private-") {
		t.Fatal("imported credential URL disclosed")
	}
}

func TestMCPOAuthJWKSRefreshThrottled(t *testing.T) {
	app, issuer := oauthTestApp(t)
	if _, err := app.AuthenticateMCPOAuthToken(context.Background(), issuer.token(t, nil)); err != nil {
		t.Fatal(err)
	}
	forged := newOAuthTestIssuer(t).token(t, map[string]any{"iss": issuer.srv.URL})
	start := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := app.AuthenticateMCPOAuthToken(context.Background(), forged); err == nil {
			t.Fatal("forged token accepted")
		}
	}
	if time.Since(start) < time.Second {
		t.Fatal("unknown signing keys can flood JWKS")
	}
	if issuer.keyRequests.Load() > 3 {
		t.Fatal("unexpected key refresh fanout")
	}
}
