package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"postra/internal/application"
	"postra/internal/domain"
)

func TestMCPOAuthCannotBypassRESTScopesOrFallBackToSession(t *testing.T) {
	app := browserTestApp(t, true)
	provider := newBrowserOIDCProvider(t)
	session, _ := browserIdentity(t, app, "oauth-rest-owner", domain.RoleAdmin)
	user, err := app.Store.GetUser(context.Background(), "oauth-rest-owner")
	if err != nil {
		t.Fatal(err)
	}
	user.AuthProvider, user.OIDCIssuer, user.OIDCSubject = "oidc", provider.server.URL, "oauth-rest-sub"
	if err := app.Store.UpdateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	admin := application.WithPrincipal(context.Background(), domain.Principal{UserID: user.ID, Role: domain.RoleAdmin, AuthMethod: "oidc"})
	if _, err := app.AdminPatchSettings(admin, application.SettingsPatch{Values: map[string]string{application.SettingOIDCIssuer: provider.server.URL, application.SettingOIDCClientID: "browser-client", application.SettingOIDCRedirectURL: "https://postra.test/auth/oidc/callback", "mcp.oauth.enabled": "true", "mcp.oauth.resource_url": "https://postra.test/mcp", "mcp.oauth.allowed_client_ids": "mcp-desktop"}}); err != nil {
		t.Fatal(err)
	}
	token := func(claims map[string]any) string {
		t.Helper()
		claims["iss"] = provider.server.URL
		claims["sub"] = user.OIDCSubject
		claims["exp"] = time.Now().Add(time.Minute).Unix()
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: provider.key}, (&jose.SignerOptions{}).WithHeader("kid", "browser-key"))
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
		result, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	h := New(app, "").Handler()
	for _, claims := range []map[string]any{
		{"typ": "Bearer", "aud": []string{"browser-client", "https://postra.test/mcp"}, "azp": "mcp-desktop", "scope": "mail.read"},
		{"typ": "Bearer", "aud": "browser-client", "azp": "mcp-desktop", "scope": "openid"},
		{"typ": "Bearer", "aud": "browser-client", "azp": "browser-client", "scope": "mail.read"},
		{"typ": "ID", "aud": "browser-client", "azp": "browser-client", "scope": "openid"},
	} {
		for _, path := range []string{"/api/accounts", "/api/v1/accounts", "/api/admin/mcp", "/api/mcp/connection"} {
			req := httptest.NewRequest("GET", "https://postra.test"+path, nil)
			raw := token(claims)
			req.Header.Set("Authorization", "Bearer "+raw)
			req.AddCookie(&http.Cookie{Name: "postra_session", Value: session})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 401 || strings.Contains(rec.Body.String(), raw) {
				t.Fatalf("delegated/ID credential escaped or leaked at %s: %d", path, rec.Code)
			}
		}
	}
	// Browser session and the distinct legacy REST access-token contract remain
	// usable. The MCP-only restriction does not disable established web SSO.
	if result := browserRequest(h, "GET", "/api/accounts", session, "", "", ""); result.Code != 200 {
		t.Fatal("browser session regressed")
	}
	req := httptest.NewRequest("GET", "https://postra.test/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+token(map[string]any{"typ": "Bearer", "aud": "browser-client", "azp": "browser-client", "scope": "openid"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("dedicated legacy REST token regressed")
	}
	// Even trusted-local deployments must not silently turn an explicit
	// delegated/invalid credential into the default local administrator.
	app.Cfg.Auth.Enabled = false
	req = httptest.NewRequest("GET", "https://postra.test/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+token(map[string]any{"typ": "Bearer", "aud": "https://postra.test/mcp", "azp": "mcp-desktop", "scope": "mail.read"}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("explicit OAuth bearer downgraded to trusted-local REST")
	}
	if rec := browserRequest(h, "GET", "/api/accounts", "", "", "", ""); rec.Code != 200 {
		t.Fatal("no-header trusted-local mode regressed")
	}
}
