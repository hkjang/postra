package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
)

func TestPasswordHashAndSessionLifecycle(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	admin, err := app.SetupInitialAdmin(ctx, "admin", "Administrator", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AuthenticateLocal(ctx, "admin", "wrong password"); err == nil {
		t.Fatal("wrong password was accepted")
	}
	authenticated, err := app.AuthenticateLocal(ctx, "admin", "correct horse battery staple")
	if err != nil || authenticated.ID != admin.ID {
		t.Fatalf("local login user=%+v err=%v", authenticated, err)
	}
	raw, csrf, session, err := app.CreateSession(ctx, admin, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || csrf == "" || session.TokenHash == raw {
		t.Fatal("session secrets were empty or stored in plaintext")
	}
	gotSession, principal, err := app.AuthenticateSession(ctx, raw)
	if err != nil || gotSession.ID != session.ID || !principal.IsAdmin() {
		t.Fatalf("session=%+v principal=%+v err=%v", gotSession, principal, err)
	}
	if err := app.Logout(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.AuthenticateSession(ctx, raw); err == nil {
		t.Fatal("logged-out session was accepted")
	}
}

func TestOIDCFlowIntegrityAndAdminSettings(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	base := WithActor(context.Background(), "test")
	admin, err := app.SetupInitialAdmin(base, "admin", "Admin", "admin-password-long")
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := WithPrincipal(base, principalFor(admin, "local"))
	if err := app.AdminSaveSettings(adminCtx, map[string]string{
		SettingOIDCIssuer:        "https://keycloak.example/realms/postra",
		SettingOIDCClientID:      "postra",
		SettingOIDCRedirectURL:   "https://postra.example/ui/auth/oidc/callback",
		SettingOIDCAutoProvision: "true",
		SettingOIDCAdminGroup:    "/postra-admins",
	}, "super-secret-client-value"); err != nil {
		t.Fatal(err)
	}
	settings, err := app.SystemSettings(adminCtx)
	if err != nil || settings[SettingOIDCSecretRef] == "" {
		t.Fatalf("OIDC secret reference missing: settings=%v err=%v", settings, err)
	}
	if strings.Contains(strings.Join(mapValues(settings), " "), "super-secret-client-value") {
		t.Fatal("OIDC client secret leaked into settings")
	}
	rt, err := app.oidcRuntime(adminCtx)
	if err != nil || rt.ClientSecret != "super-secret-client-value" {
		t.Fatalf("encrypted OIDC secret could not be resolved: runtime=%+v err=%v", rt, err)
	}

	flow := OIDCFlow{State: "state", Nonce: "nonce", CodeVerifier: "verifier", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	signed, err := app.SignOIDCFlow(flow)
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.VerifyOIDCFlow(signed)
	if err != nil || got.State != flow.State {
		t.Fatalf("verified flow=%+v err=%v", got, err)
	}
	if _, err := app.VerifyOIDCFlow(signed + "tampered"); err == nil {
		t.Fatal("tampered OIDC flow cookie was accepted")
	}
}

func TestOIDCStateKeyIsSharedAcrossAppInstances(t *testing.T) {
	app, pop, smtp, aiProvider := newTestApp(t)
	second, err := New(app.Cfg, app.Store, app.Objects, app.Secrets, pop, smtp, aiProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown()
	if app.oidcStateKey != second.oidcStateKey {
		t.Fatal("OIDC state key differs across instances sharing the same database")
	}
	flow := OIDCFlow{State: "shared-state", Nonce: "nonce", CodeVerifier: "verifier", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	signed, err := app.SignOIDCFlow(flow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.VerifyOIDCFlow(signed); err != nil {
		t.Fatalf("another pod could not verify OIDC flow: %v", err)
	}
}

func mapValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func TestAdminManagementAndTenantIsolation(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	base := WithActor(context.Background(), "test")
	admin, err := app.SetupInitialAdmin(base, "admin", "Admin", "admin-password-long")
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := WithPrincipal(base, principalFor(admin, "local"))
	user, err := app.AdminCreateUser(adminCtx, CreateUserInput{
		LoginID: "member", DisplayName: "Member", Role: domain.RoleUser, Password: "member-password-long",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AdminUpdateUser(adminCtx, admin.ID, domain.RoleUser, domain.UserActive); err == nil {
		t.Fatal("last active admin could be demoted")
	}

	adminAccount := createScopedAccount(t, app, adminCtx, "admin@example.test")
	userCtx := WithPrincipal(base, principalFor(user, "local"))
	userAccount := createScopedAccount(t, app, userCtx, "member@example.test")
	adminAccounts, err := app.ListAccounts(adminCtx)
	if err != nil || len(adminAccounts) != 1 || adminAccounts[0].ID != adminAccount.ID {
		t.Fatalf("admin accounts=%+v err=%v", adminAccounts, err)
	}
	userAccounts, err := app.ListAccounts(userCtx)
	if err != nil || len(userAccounts) != 1 || userAccounts[0].ID != userAccount.ID {
		t.Fatalf("user accounts=%+v err=%v", userAccounts, err)
	}
	if _, err := app.GetAccount(userCtx, adminAccount.ID); err == nil {
		t.Fatal("user could access another tenant's account")
	}
	renamed, newEmail := "Renamed account", "renamed@example.test"
	if _, err := app.UpdateAccount(adminCtx, UpdateAccountInput{
		AccountID: adminAccount.ID, Name: &renamed, Email: &newEmail,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := app.GetAccount(adminCtx, adminAccount.ID)
	if err != nil || got.Name != renamed || got.Email != newEmail {
		t.Fatalf("updated account=%+v err=%v", got, err)
	}
	if err := app.DeleteAccount(adminCtx, adminAccount.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetAccount(adminCtx, adminAccount.ID); err == nil {
		t.Fatal("deleted account remained accessible")
	}
	if err := app.AdminDeleteUser(adminCtx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.AuthenticateLocal(base, "member", "member-password-long"); err == nil {
		t.Fatal("deleted user could still sign in")
	}
}

func TestAdminAISettingsAndConnectionProbe(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	base := WithActor(context.Background(), "test")
	admin, err := app.SetupInitialAdmin(base, "admin", "Admin", "admin-password-long")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(base, principalFor(admin, "local"))
	if err := app.AdminSaveAISettings(ctx, map[string]string{
		SettingAIBaseURL:         "http://127.0.0.1:11434/v1",
		SettingAIModel:           "mail-model",
		SettingAIEmbedModel:      "embed-model",
		SettingAITimeout:         "30",
		SettingAIMaxTokens:       "1024",
		SettingAIAllowExternal:   "false",
		SettingAIMaskExternalPII: "true",
	}, "encrypted-ai-key"); err != nil {
		t.Fatal(err)
	}
	settings, err := app.SystemSettings(ctx)
	if err != nil || settings[SettingAIModel] != "mail-model" || settings[SettingAIAPIKeyRef] == "" {
		t.Fatalf("settings=%v err=%v", settings, err)
	}
	if strings.Contains(strings.Join(mapValues(settings), " "), "encrypted-ai-key") {
		t.Fatal("AI API key leaked into settings")
	}
	result, err := app.AdminTestAI(ctx)
	if err != nil || result.Model != "mail-model" {
		t.Fatalf("AI probe=%+v err=%v", result, err)
	}
	if err := app.AdminSaveAISettings(ctx, map[string]string{
		SettingAIBaseURL: "http://127.0.0.1:11434/v1", SettingAIModel: "mail-model",
		SettingAIAPIKeyRef: "",
	}, ""); err != nil {
		t.Fatal(err)
	}
	settings, _ = app.SystemSettings(ctx)
	if settings[SettingAIAPIKeyRef] != "" {
		t.Fatal("AI API key reference was not removed")
	}
}

func createScopedAccount(t *testing.T, app *App, ctx context.Context, email string) *domain.MailAccount {
	t.Helper()
	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, email,
		domain.NewSecretHandle([]byte("mail-password")))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := app.CreateAccount(ctx, CreateAccountInput{
		Name: email, Email: email, POP3Host: "127.0.0.1", POP3Security: "none",
		POP3Username: email, POP3SecretRef: string(ref), SMTPHost: "127.0.0.1",
		SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestMCPKeyLifecycleAndAdminControl(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	base := WithActor(context.Background(), "test")
	admin, err := app.SetupInitialAdmin(base, "admin", "Admin", "admin-password-long")
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := WithPrincipal(base, principalFor(admin, "local"))

	// Create user
	u2, err := app.AdminCreateUser(adminCtx, CreateUserInput{
		LoginID: "user2", DisplayName: "User 2", Password: "user2-password-long", Role: domain.RoleUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	user2Ctx := WithPrincipal(base, principalFor(u2, "local"))

	// Create MCP keys for admin and user2
	adminKey, adminRawKey, err := app.CreateMCPKey(adminCtx, "Admin Laptop")
	if err != nil || adminKey == nil || adminRawKey == "" {
		t.Fatalf("CreateMCPKey admin error: %v", err)
	}
	u2Key, u2RawKey, err := app.CreateMCPKey(user2Ctx, "User2 Claude")
	if err != nil || u2Key == nil || u2RawKey == "" {
		t.Fatalf("CreateMCPKey user2 error: %v", err)
	}

	// Test AuthenticateMCPKey
	_, pAdmin, err := app.AuthenticateMCPKey(base, adminRawKey)
	if err != nil || pAdmin.UserID != admin.ID || pAdmin.AuthMethod != "mcp_key" || !pAdmin.IsAdmin() {
		t.Fatalf("AuthenticateMCPKey admin error: %v, principal: %+v", err, pAdmin)
	}
	_, pUser2, err := app.AuthenticateMCPKey(base, u2RawKey)
	if err != nil || pUser2.UserID != u2.ID || pUser2.AuthMethod != "mcp_key" || pUser2.IsAdmin() {
		t.Fatalf("AuthenticateMCPKey user2 error: %v, principal: %+v", err, pUser2)
	}

	// List user's own keys
	myKeys, err := app.ListMyMCPKeys(user2Ctx)
	if err != nil || len(myKeys) != 1 || myKeys[0].ID != u2Key.ID {
		t.Fatalf("ListMyMCPKeys error: %v, keys: %+v", err, myKeys)
	}

	// Admin list all keys
	allKeys, err := app.AdminListMCPKeys(adminCtx)
	if err != nil || len(allKeys) < 2 {
		t.Fatalf("AdminListMCPKeys error: %v, keys: %+v", err, allKeys)
	}

	// Admin revokes user2's key
	if err := app.AdminRevokeMCPKey(adminCtx, u2Key.ID); err != nil {
		t.Fatalf("AdminRevokeMCPKey error: %v", err)
	}

	// User2's revoked key can no longer authenticate
	if _, _, err := app.AuthenticateMCPKey(base, u2RawKey); err == nil {
		t.Fatal("revoked MCP key was successfully authenticated")
	}
}
