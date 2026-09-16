package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/transport/httpapi/contracts"
)

func TestMCPConnectionInstructionsRequireAuthenticationAndNeverReturnCredentials(t *testing.T) {
	app := browserTestApp(t, true)
	admin, _ := browserIdentity(t, app, "oauth-admin", domain.RoleAdmin)
	user, _ := browserIdentity(t, app, "oauth-member", domain.RoleUser)
	adminCtx := application.WithPrincipal(context.Background(), domain.Principal{UserID: "oauth-admin", Role: domain.RoleAdmin, AuthMethod: "local"})
	if _, err := app.AdminPatchSettings(adminCtx, application.SettingsPatch{Values: map[string]string{
		application.SettingOIDCIssuer: "https://sso.corp.test/realms/postra", application.SettingOIDCClientID: "postra-web", application.SettingOIDCRedirectURL: "https://postra.test/auth/oidc/callback",
		"mcp.oauth.enabled": "true", "mcp.oauth.resource_url": "https://postra.test/mcp", "mcp.oauth.allowed_client_ids": "desktop", "mcp.oauth.allowed_scopes": "mail.read,mail.search", "mcp.permissions.search": "false",
	}}); err != nil {
		t.Fatal(err)
	}
	_, apiKey, err := app.CreateMCPKey(adminCtx, "private-key-name")
	if err != nil {
		t.Fatal(err)
	}
	handler := New(app, "private-deployment-token").Handler()
	for _, prefix := range []string{"/api", "/api/v1"} {
		path := prefix + "/mcp/connection"
		if rec := browserRequest(handler, "GET", path, "", "", "", ""); rec.Code != 401 {
			t.Fatalf("public connection instructions exposed: %d", rec.Code)
		}
		for _, identity := range []string{admin, user} {
			rec := browserRequest(handler, "GET", path, identity, "", "", "")
			var info application.MCPOAuthConnectionInfo
			if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || rec.Code != 200 || info.ActiveEndpoint != "/mcp" || info.PendingRestart || !info.OAuth.Enabled || !info.OAuth.Configured || info.OAuth.ResourceURL != "https://postra.test/mcp" || info.OAuth.MetadataURL != "https://postra.test/.well-known/oauth-protected-resource/mcp" {
				t.Fatalf("incorrect connection instructions: status=%d err=%v", rec.Code, err)
			}
			var wire any
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if err := contracts.Validate("MCPOAuthConnectionInfo", wire); err != nil {
				t.Fatal(err)
			}
			if len(info.OAuth.ScopesSupported) != 1 || info.OAuth.ScopesSupported[0] != "mail.read" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("live policy or private cache control missing")
			}
			for _, secret := range []string{admin, user, apiKey, "private-deployment-token", "private-key-name", "client_secret", "api_key"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatal("credential data included in connection instructions")
				}
			}
		}
		req := httptest.NewRequest("GET", "https://untrusted-host.test"+path, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "untrusted-host.test") {
			t.Fatal("API-key discovery or configured-origin policy failed")
		}
	}
}
