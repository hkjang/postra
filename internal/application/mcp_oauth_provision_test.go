package application

import (
	"context"
	"strings"
	"sync"
	"testing"

	"postra/internal/domain"
)

// A Keycloak identity with no Postra user yet is connected on its first MCP
// request by the same rules as a first browser SSO login, instead of being
// refused until the person opens the web UI once.
func TestMCPOAuthConnectsAnUnlinkedKeycloakIdentity(t *testing.T) {
	app, issuer := oauthTestApp(t)
	ctx := context.Background()
	lookup := func(sub string) *domain.User {
		t.Helper()
		u, err := app.Store.GetUserByOIDC(ctx, issuer.srv.URL, sub)
		if err != nil {
			t.Fatalf("no user linked to %s: %v", sub, err)
		}
		return u
	}

	t.Run("creates the account when SSO users may be created", func(t *testing.T) {
		p, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, map[string]any{"sub": "newcomer-sub", "preferred_username": "newcomer", "email": "newcomer@example.test", "name": "New Comer"}))
		if err != nil {
			t.Fatal(err)
		}
		u := lookup("newcomer-sub")
		if p.UserID != u.ID || u.LoginID != "newcomer" || u.Email != "newcomer@example.test" || u.DisplayName != "New Comer" ||
			u.AuthProvider != "oidc" || u.Role != domain.RoleUser || u.Status != domain.UserActive {
			t.Fatalf("provisioned user = %+v, principal %+v", u, p)
		}
		// The token still narrows the account: scopes are the token's, not the user's.
		if !p.IsMCPScoped() || p.AuthMethod != "mcp_oauth" {
			t.Fatalf("provisioned principal lost its token scoping: %+v", p)
		}
		again, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, map[string]any{"sub": "newcomer-sub", "preferred_username": "newcomer"}))
		if err != nil || again.UserID != u.ID {
			t.Fatalf("a second request did not reuse the account: %+v %v", again, err)
		}
	})

	t.Run("links the local account the person clearly owns", func(t *testing.T) {
		local := &domain.User{ID: "local-person", LoginID: "local-person", Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "local", Email: "local@example.test"}
		if err := app.Store.CreateUser(ctx, local, ""); err != nil {
			t.Fatal(err)
		}
		p, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, map[string]any{"sub": "local-sub", "email": "local@example.test"}))
		if err != nil || p.UserID != "local-person" {
			t.Fatalf("existing local account not linked: %+v %v", p, err)
		}
		if lookup("local-sub").ID != "local-person" {
			t.Fatal("link was not persisted")
		}
	})

	t.Run("never takes over an account federated to another subject", func(t *testing.T) {
		// oauth-owner is already linked to subject-owner with this email.
		p, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, map[string]any{"sub": "impostor-sub", "email": "owner@example.test"}))
		if err == nil && p.UserID == "oauth-owner" {
			t.Fatal("a different Keycloak subject reached an account federated to another one")
		}
		if owner, _ := app.Store.GetUserByOIDC(ctx, issuer.srv.URL, "subject-owner"); owner == nil || owner.ID != "oauth-owner" {
			t.Fatal("the original federation was rewritten")
		}
	})

	t.Run("a burst of first requests creates one account", func(t *testing.T) {
		const n = 8
		ids := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, map[string]any{"sub": "burst-sub", "preferred_username": "burst"}))
				ids[i], errs[i] = p.UserID, err
			}()
		}
		wg.Wait()
		for i := range n {
			if errs[i] != nil || ids[i] == "" || ids[i] != ids[0] {
				t.Fatalf("concurrent first requests disagreed: ids=%v errs=%v", ids, errs)
			}
		}
		users, err := app.Store.ListUsers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, u := range users {
			if u.OIDCSubject == "burst-sub" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%d accounts created for one identity", count)
		}
	})

	t.Run("refuses with the blocking policy when SSO users may not be created", func(t *testing.T) {
		if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: map[string]string{SettingOIDCAutoProvision: "false"}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: map[string]string{SettingOIDCAutoProvision: "true"}})
		})
		_, reason, err := app.AuthenticateMCPOAuthTokenReason(ctx, issuer.token(t, map[string]any{"sub": "blocked-sub", "preferred_username": "blocked"}))
		if err == nil || !strings.Contains(reason, "auth.oidc.auto_provision") {
			t.Fatalf("unlinkable identity accepted or refused without naming the policy: %q %v", reason, err)
		}
		if _, err := app.Store.GetUserByOIDC(ctx, issuer.srv.URL, "blocked-sub"); err == nil {
			t.Fatal("an account was created while creation is off")
		}
		// Linking an owned account is not creating one, so it still works.
		local := &domain.User{ID: "local-two", LoginID: "local-two", Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "local", Email: "two@example.test"}
		if err := app.Store.CreateUser(ctx, local, ""); err != nil {
			t.Fatal(err)
		}
		if p, err := app.AuthenticateMCPOAuthToken(ctx, issuer.token(t, map[string]any{"sub": "two-sub", "email": "two@example.test"})); err != nil || p.UserID != "local-two" {
			t.Fatalf("linking stopped with creation off: %+v %v", p, err)
		}
	})

	t.Run("a disabled linked user stays refused", func(t *testing.T) {
		u := lookup("newcomer-sub")
		u.Status = domain.UserDisabled
		if err := app.Store.UpdateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		_, reason, err := app.AuthenticateMCPOAuthTokenReason(ctx, issuer.token(t, map[string]any{"sub": "newcomer-sub"}))
		if err == nil || !strings.Contains(reason, "not active") {
			t.Fatalf("disabled user accepted: %q %v", reason, err)
		}
	})
}
