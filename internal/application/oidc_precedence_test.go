package application

import (
	"context"
	"testing"
)

func TestOIDCAdministratorSecretOverridesAndExplicitClear(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	app.Cfg.Auth.OIDCClientSecret = "fixture-deployment-secret"
	ctx := context.Background()
	runtime, err := app.oidcRuntime(ctx)
	if err != nil || runtime.ClientSecret != "fixture-deployment-secret" {
		t.Fatal("deployment secret fallback was not used before any override")
	}
	admin, err := app.SetupInitialAdmin(ctx, "oidc-fixture-admin", "Admin", "fixture-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithPrincipal(ctx, principalFor(admin, "local"))
	if err := app.AdminSaveSettings(ctx, map[string]string{}, "fixture-admin-secret"); err != nil {
		t.Fatal(err)
	}
	runtime, err = app.oidcRuntime(ctx)
	if err != nil || runtime.ClientSecret != "fixture-admin-secret" || runtime.SecretRef == "" {
		t.Fatal("saved administrator secret did not override deployment secret")
	}
	if err := app.AdminSaveSettings(ctx, map[string]string{SettingOIDCSecretRef: ""}, ""); err != nil {
		t.Fatal(err)
	}
	runtime, err = app.oidcRuntime(ctx)
	if err != nil || runtime.ClientSecret != "" {
		t.Fatal("explicit administrator clear resurrected a deployment secret")
	}
	if err := app.Store.UpsertSettings(ctx, map[string]string{SettingOIDCSecretRef: "missing-fixture-secret"}); err != nil {
		t.Fatal(err)
	}
	if runtime, err := app.oidcRuntime(ctx); err == nil || runtime.ClientSecret != "" {
		t.Fatal("unavailable administrator secret fell back to old deployment credential")
	}
}
