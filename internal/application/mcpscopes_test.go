package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"postra/internal/domain"
)

func TestMCPKeyScopesPersistAndCannotEscalate(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	base := context.Background()
	admin, err := app.SetupInitialAdmin(base, "scope-admin", "Admin", "scope-admin-long-password")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(base, principalFor(admin, "local"))
	key, raw, err := app.CreateMCPKey(ctx, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if len(key.Scopes) != 2 || !contains(key.Scopes, "mail.read") || !contains(key.Scopes, "mail.search") {
		t.Fatalf("unsafe default scopes: %v", key.Scopes)
	}
	_, principal, err := app.AuthenticateMCPKey(base, raw)
	if err != nil {
		t.Fatal(err)
	}
	keyCtx := WithPrincipal(base, principal)
	if err := CheckMCPScopes(keyCtx, "mail.send"); err == nil {
		t.Fatal("default key can send")
	}
	if _, _, err := app.CreateMCPKeyWithScopes(keyCtx, "escalated", MCPScopes); err == nil {
		t.Fatal("MCP credential minted a higher-privilege key")
	}
	if _, err := app.UpdateMCPKeyScopes(keyCtx, key.ID, MCPScopes, true); err == nil {
		t.Fatal("MCP credential edited its own privileges")
	}
	if _, err := app.UpdateMCPKeyScopes(ctx, key.ID, []string{}, false); err != nil {
		t.Fatal(err)
	}
	stored, principal, err := app.AuthenticateMCPKey(base, raw)
	if err != nil || stored.LegacyScopes || len(stored.Scopes) != 0 {
		t.Fatalf("explicit empty lost: %+v %v", stored, err)
	}
	if err := CheckMCPScopes(WithPrincipal(base, principal), "mail.read"); err == nil {
		t.Fatal("empty scope array recovered default read permissions")
	}
	if _, _, err := app.CreateMCPKeyWithScopes(ctx, "invalid", []string{"mail.*"}); err == nil {
		t.Fatal("wildcard scope accepted")
	}
}

func TestMCPScopesCannotOverrideRolesOrGlobalPolicy(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	app.applyRuntimeSettings(map[string]string{"mcp.permissions.delete": "true"})
	ctx := WithPrincipal(context.Background(), domain.Principal{UserID: DefaultUserID, Role: domain.RoleUser, AuthMethod: "mcp_key", MCPKeyID: "key", MCPScopes: MCPScopes})
	if err := app.CheckMCPToolPolicy(ctx, "mail_local_delete"); err == nil {
		t.Fatal("scopes overrode role-level delete denial")
	}
	if err := app.CheckMCPToolPolicy(ctx, "mail_send"); err != nil {
		t.Fatal(err)
	}
	app.applyRuntimeSettings(map[string]string{"mcp.permissions.send": "false"})
	if err := app.CheckMCPToolPolicy(ctx, "mail_send"); err == nil {
		t.Fatal("global capability deny ignored")
	}
	app.applyRuntimeSettings(map[string]string{"mcp.permissions.draft": "false"})
	if err := app.CheckMCPToolPolicy(ctx, "mail_draft"); err == nil {
		t.Fatal("alias bypassed canonical capability deny")
	}
	app.applyRuntimeSettings(map[string]string{"mcp.enabled": "false"})
	if err := app.CheckMCPToolPolicy(ctx, "mail_search"); err == nil {
		t.Fatal("global disable ignored")
	}
}

func TestMCPPolicyValidation(t *testing.T) {
	for _, raw := range []string{`{"role_max_level":{"user":"superuser"}}`, `{"deny_tool":["mail_send"]}`, `{}`, `{"deny_tools":["mail_send"]} {}`} {
		err := ValidateMCPPolicySettings(raw)
		if (raw == `{}`) != (err == nil) {
			t.Errorf("unexpected validation %q: %v", raw, err)
		}
	}
}

func TestPublicErrorContractDoesNotExposeUnknownFailures(t *testing.T) {
	ctx := WithRequestTrace(context.Background(), "trace_test_12345")
	for _, err := range []error{errors.New("database password=top-secret; api_key=also-secret"), context.DeadlineExceeded, domain.ErrNotFound, &domain.PublicError{Code: "forbidden", Message: "권한 없음", Status: 403}} {
		status, response := PublicError(ctx, err)
		if status < 400 || response.TraceID != "trace_test_12345" || response.Code == "" || response.Message == "" {
			t.Fatalf("incomplete public failure: %+v", response)
		}
		raw, _ := json.Marshal(response)
		if strings.Contains(string(raw), "secret") {
			t.Fatal("private provider text exposed")
		}
	}
	if RequestTrace(WithRequestTrace(ctx, "\r\nforged secret")) == "\r\nforged secret" {
		t.Fatal("unbounded trace reflected")
	}
}
