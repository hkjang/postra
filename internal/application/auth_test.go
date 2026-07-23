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
