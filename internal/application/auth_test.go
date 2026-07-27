package application

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	aiadapter "postra/internal/adapters/ai"
	"postra/internal/domain"
	"postra/internal/platform/config"
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

func TestAdminPreprovisionsOIDCUserAndSharedMailSecret(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	app.IMAP = &fakeInbound{raw: map[string]string{
		"999.1": testMail("sso-auto-1", "SSO auto collected", "automatic IMAP sync"),
	}}
	base := WithActor(context.Background(), "test")
	admin, err := app.SetupInitialAdmin(base, "admin", "Admin", "admin-password-long")
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := WithPrincipal(base, principalFor(admin, "local"))
	if err := app.AdminSaveSettings(adminCtx, map[string]string{
		SettingOIDCIssuer:      "https://keycloak.example/realms/postra",
		SettingOIDCClientID:    "postra",
		SettingOIDCRedirectURL: "https://postra.example/ui/auth/oidc/callback",
	}, ""); err != nil {
		t.Fatal(err)
	}
	legacyDeleted := &domain.User{
		ID: "usr_legacy_deleted", LoginID: "hong@corp.local", DisplayName: "Old Hong",
		Email: "hong@corp.local", Role: domain.RoleUser, Status: domain.UserDeleted,
		AuthProvider: "oidc", OIDCIssuer: "https://keycloak.example/realms/postra",
		OIDCSubject: "old-subject",
	}
	if err := app.Store.CreateUser(base, legacyDeleted, ""); err != nil {
		t.Fatal(err)
	}
	result, err := app.AdminProvisionMailAccount(adminCtx, AdminMailProvisionInput{
		Email: "hong@corp.local", IMAPHost: "127.0.0.1",
		IMAPPort: 993, IMAPSecurity: "tls", SMTPHost: "127.0.0.1",
		SMTPPort: 587, SMTPSecurity: "starttls", AuthUsername: "hong",
		MailPassword: "mail-password-only", ApplyToAllUsers: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.AuthProvider != "oidc" || result.User.OIDCSubject != "" {
		t.Fatalf("expected pending OIDC link, got %+v", result.User)
	}
	old, err := app.Store.GetUser(base, legacyDeleted.ID)
	if err != nil || old.Email != "" || old.OIDCSubject != "" || old.LoginID == "hong@corp.local" {
		t.Fatalf("legacy deleted identity was not released: user=%+v err=%v", old, err)
	}
	if result.Account.UserID != result.User.ID || result.Account.InboundProtocol != domain.InboundIMAP {
		t.Fatalf("account ownership/protocol mismatch: %+v", result.Account)
	}
	if result.Account.POP3Secret == "" || result.Account.POP3Secret != result.Account.SMTPSecret {
		t.Fatal("IMAP and SMTP must share one secret by default")
	}
	if result.Account.Email != "hong@corp.local" || result.Account.POP3Username != "hong" {
		t.Fatalf("mail defaults were not applied: %+v", result.Account)
	}
	separate := false
	noAuth, err := app.AdminProvisionMailAccount(adminCtx, AdminMailProvisionInput{
		Email: "hong@corp.local", IMAPHost: "127.0.0.1", IMAPSecurity: "tls",
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
		MailPassword: "imap-only-password", SamePassword: &separate,
	})
	if err != nil {
		t.Fatalf("SMTP without AUTH must not require an SMTP password: %v", err)
	}
	if noAuth.Account.SMTPAuth != "none" {
		t.Fatalf("expected SMTP AUTH to be disabled, got %q", noAuth.Account.SMTPAuth)
	}

	linked := app.linkExistingLocalUser(base,
		oidcRuntime{Issuer: "https://keycloak.example/realms/postra"},
		oidcClaims{Subject: "kc-subject-1", Email: "hong@corp.local"})
	if linked == nil || linked.ID != result.User.ID || linked.OIDCSubject != "kc-subject-1" {
		t.Fatalf("first Keycloak login did not complete pending link: %+v", linked)
	}

	policy, err := app.AdminProvisionMailAccount(adminCtx, AdminMailProvisionInput{
		IMAPHost: "127.0.0.1", IMAPSecurity: "tls", SMTPHost: "127.0.0.1",
		SMTPSecurity: "none", SMTPAuth: "none", MailPassword: "organization-password",
	})
	if err != nil || policy.User != nil || policy.Account != nil {
		t.Fatalf("targetless policy failed: result=%+v err=%v", policy, err)
	}
	autoUser := &domain.User{
		ID: "usr_auto", LoginID: "auto", DisplayName: "Auto", Email: "auto@corp.local",
		Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "oidc",
		OIDCIssuer: "https://keycloak.example/realms/postra", OIDCSubject: "auto-sub",
	}
	if err := app.Store.CreateUser(base, autoUser, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.autoProvisionOIDCMail(base, autoUser); err != nil {
		t.Fatal(err)
	}
	autoCtx := WithPrincipal(base, principalFor(autoUser, "oidc"))
	autoAccounts, err := app.ListAccounts(autoCtx)
	if err != nil || len(autoAccounts) != 1 || autoAccounts[0].Email != autoUser.Email {
		t.Fatalf("automatic SSO mail provisioning failed: accounts=%+v err=%v", autoAccounts, err)
	}
	if autoAccounts[0].SMTPAuth != "none" {
		t.Fatalf("offline SMTP relay unexpectedly requires authentication: %+v", autoAccounts[0])
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		messages, searchErr := app.Search(autoCtx, domain.SearchQuery{AccountID: autoAccounts[0].ID, Limit: 10})
		if searchErr == nil && len(messages.Messages) == 1 &&
			messages.Messages[0].Subject == "SSO auto collected" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first SSO login did not immediately collect IMAP mail: result=%+v err=%v", messages, searchErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	draft, err := app.CreateDraft(autoCtx, CreateDraftInput{
		AccountID: autoAccounts[0].ID, Kind: "new", To: []string{"recipient@corp.local"},
		Subject: "SMTP relay", Body: "no auth",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, approval, err := app.RequestSendApproval(autoCtx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Send(autoCtx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: approval.Token}); err != nil {
		t.Fatalf("SMTP relay without authentication failed: %v", err)
	}
	if len(smtp.sent) != 1 || smtp.lastOpts.AuthMethod != "none" || smtp.lastOpts.Password != nil {
		t.Fatalf("SMTP AUTH was not skipped: sent=%d opts=%+v", len(smtp.sent), smtp.lastOpts)
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
	if _, err := app.GetAccount(adminCtx, userAccount.ID); err == nil {
		t.Fatal("administrator could access another user's private mail account")
	}
	privateMessage := &domain.Message{
		ID: "msg_private_member", UserID: user.ID, AccountID: userAccount.ID, UIDL: "private-1",
		Subject: "private", From: domain.Address{Email: "sender@example.test"},
		RawHash: "private-hash", RawURI: "mem://private", Date: 1, CreatedAt: 1,
	}
	if err := app.Store.InsertMessage(base, privateMessage,
		&domain.MessageBody{MessageID: privateMessage.ID, TextBody: "private body"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetMessage(adminCtx, privateMessage.ID, true); err == nil {
		t.Fatal("administrator could read another user's private message")
	}
	if got, err := app.GetMessage(userCtx, privateMessage.ID, true); err != nil ||
		got.Body == nil || got.Body.TextBody != "private body" {
		t.Fatalf("owner could not read private message: view=%+v err=%v", got, err)
	}
	if err := app.AdminPurgeDeletedUserMail(adminCtx, "member@example.test", "member@example.test"); err == nil {
		t.Fatal("active user's mailbox could be purged")
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
	users, err := app.AdminListUsers(adminCtx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range users {
		if users[i].ID == user.ID {
			t.Fatal("deleted user remained visible in administrator list")
		}
	}
	if err := app.AdminPurgeDeletedUserMail(adminCtx, "member@example.test", "wrong@example.test"); err == nil {
		t.Fatal("mail purge accepted a mismatched confirmation")
	}
	if err := app.AdminPurgeDeletedUserMail(adminCtx, "member@example.test", "member@example.test"); err != nil {
		t.Fatalf("deleted user's old mailbox could not be purged: %v", err)
	}
	if _, err := app.Store.GetAccount(base, user.ID, userAccount.ID); err == nil {
		t.Fatal("purged deleted-user account still exists")
	}
	if _, err := app.Store.GetMessage(base, user.ID, privateMessage.ID); err == nil {
		t.Fatal("purged deleted-user message still exists")
	}
	if _, err := app.AdminCreateUser(adminCtx, CreateUserInput{
		LoginID: "member", DisplayName: "Replacement", Role: domain.RoleUser,
		Password: "replacement-password-long",
	}); err != nil {
		t.Fatalf("deleted identity could not be freshly provisioned: %v", err)
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

func TestAIKeyAndConfigSurviveRestartWithoutErrorLeak(t *testing.T) {
	app, pop, smtp, _ := newTestApp(t)
	base := WithActor(context.Background(), "test")
	admin, err := app.SetupInitialAdmin(base, "admin", "Admin", "admin-password-long")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(base, principalFor(admin, "local"))
	const apiKey = "sk-restart-secret-123456789"
	fail := false
	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api_key=` + apiKey + ` Authorization: Bearer ` + apiKey + `"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"provider is responding normally"}}]}`))
	}))
	defer srv.Close()
	if err := app.AdminSaveAISettings(ctx, map[string]string{
		SettingAIBaseURL: srv.URL, SettingAIModel: "restart-model",
		SettingAITimeout: "5", SettingAIMaxTokens: "64",
	}, apiKey); err != nil {
		t.Fatal(err)
	}

	freshCfg := config.Default()
	freshCfg.DataDir = app.Cfg.DataDir
	provider := aiadapter.New(freshCfg.AI, app.Secrets)
	restarted, err := New(freshCfg, app.Store, app.Objects, app.Secrets, pop, smtp, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown()
	result, err := restarted.AdminTestAI(ctx)
	if err != nil || !result.OK || result.Model != "restart-model" {
		t.Fatalf("restarted AI test failed: result=%+v err=%v", result, err)
	}
	if authorization != "Bearer "+apiKey {
		t.Fatalf("persisted AI key was not restored; authorization=%q", authorization)
	}

	fail = true
	result, err = restarted.AdminTestAI(ctx)
	if err != nil || result.OK {
		t.Fatalf("invalid-key response was not reported as failure: result=%+v err=%v", result, err)
	}
	if strings.Contains(result.Message, apiKey) || !strings.Contains(result.Message, "[REDACTED]") {
		t.Fatalf("AI key leaked through connection error: %s", result.Message)
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

func TestEnsureUserReRunAfterAdminSetup(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")

	if _, err := app.SetupInitialAdmin(ctx, "admin", "Administrator", "a-secure-password-123"); err != nil {
		t.Fatalf("SetupInitialAdmin error: %v", err)
	}

	if err := app.Store.EnsureUser(ctx, DefaultUserID, "local"); err != nil {
		t.Fatalf("EnsureUser re-run failed: %v", err)
	}
}
