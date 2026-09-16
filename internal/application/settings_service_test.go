package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"postra/internal/domain"
)

func settingField(t *testing.T, view SettingsView, key string) EffectiveSetting {
	t.Helper()
	for _, field := range view.Fields {
		if field.Key == key {
			return field
		}
	}
	t.Fatalf("missing setting %s", key)
	return EffectiveSetting{}
}
func settingsAdmin() context.Context {
	return WithPrincipal(context.Background(), domain.Principal{UserID: DefaultUserID, Role: domain.RoleAdmin, AuthMethod: "local"})
}

func TestSettingsLayeringLocksAndUserIsolation(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	admin := settingsAdmin()
	if err := app.Store.EnsureUser(admin, "prefs-user", "prefs-user"); err != nil {
		t.Fatal(err)
	}
	user := WithPrincipal(context.Background(), domain.Principal{UserID: "prefs-user", Role: domain.RoleUser})
	if _, err := app.AdminSettingsCatalog(user); err == nil {
		t.Fatal("nonadmin saw operational settings")
	}
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{"ui.theme": "dark", "compose.tone": "formal"}}); err != nil {
		t.Fatal(err)
	}
	view, err := app.PersonalSettings(user, "")
	if err != nil {
		t.Fatal(err)
	}
	if field := settingField(t, view, "ui.theme"); field.Value != "dark" || field.Source != "admin" {
		t.Fatalf("bad admin inheritance: %+v", field)
	}
	view, err = app.SavePersonalSettings(user, "", SettingsPatch{Values: map[string]string{"ui.theme": "light"}, Revision: view.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if field := settingField(t, view, "ui.theme"); field.Value != "light" || field.Source != "user" {
		t.Fatalf("bad personal override: %+v", field)
	}
	other, _ := app.PersonalSettings(admin, "")
	if settingField(t, other, "ui.theme").Value != "dark" {
		t.Fatal("user preference leaked between identities")
	}
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Locks: map[string]bool{"ui.theme": true}}); err != nil {
		t.Fatal(err)
	}
	view, _ = app.PersonalSettings(user, "")
	if field := settingField(t, view, "ui.theme"); field.Value != "dark" || !field.Locked {
		t.Fatalf("policy bypass: %+v", field)
	}
	for _, patch := range []SettingsPatch{{Values: map[string]string{"ui.theme": "light"}}, {Reset: []string{"ui.theme"}}, {Values: map[string]string{"ai.base_url": "https://attacker.invalid"}}} {
		if _, err := app.SavePersonalSettings(user, "", patch); err == nil {
			t.Fatalf("forbidden write accepted: %+v", patch)
		}
	}
}

func TestSettingsLiveAndRestartSemantics(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	view, err := app.AdminSettingsCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldSession := settingField(t, view, "mcp.session_timeout_sec").Value
	oldAddr := app.Cfg.HTTPAddr
	changed, err := app.AdminPatchSettings(ctx, SettingsPatch{Revision: view.Revision, Values: map[string]string{"send.max_per_minute": "7", "sync.auto_sync_minutes": "3", "ai.temperature": "0.7", "ai.context_length": "64000", "ai.auto_context_length": "false", "system.http_addr": "127.0.0.1:8888", "mcp.session_timeout_sec": "900"}})
	if err != nil {
		t.Fatal(err)
	}
	if app.EffectiveConfig().Send.MaxPerMinute != 7 || app.EffectiveConfig().Sync.AutoSyncMinutes != 3 || app.currentAIConfig().Temperature != .7 {
		t.Fatal("live settings were not applied")
	}
	if app.currentAIConfig().AutoContextLength || app.currentAIConfig().ContextLength != 64000 || settingField(t, changed, "ai.auto_context_length").Apply != "live" {
		t.Fatal("AI context mode was not applied live")
	}
	if app.EffectiveConfig().HTTPAddr != oldAddr {
		t.Fatal("restart setting was incorrectly hot applied")
	}
	if field := settingField(t, changed, "mcp.session_timeout_sec"); !field.PendingRestart || field.ActiveValue != oldSession {
		t.Fatalf("missing restart diff: %+v", field)
	}
	if _, err := app.AdminPatchSettings(ctx, SettingsPatch{Revision: view.Revision, Values: map[string]string{"send.max_per_minute": "9"}}); err == nil {
		t.Fatal("stale revision overwrote another operator")
	}
	app2, err := New(app.initialConfig, app.Store, app.Objects, app.Secrets, app.POP3, app.SMTP, app.aiRaw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app2.Shutdown)
	if app2.EffectiveConfig().HTTPAddr != "127.0.0.1:8888" || app2.EffectiveConfig().Send.MaxPerMinute != 7 {
		t.Fatal("restart lost persisted settings")
	}
	if app2.currentAIConfig().AutoContextLength || app2.currentAIConfig().ContextLength != 64000 {
		t.Fatal("restart lost persisted AI context settings")
	}
	view2, _ := app2.AdminSettingsCatalog(ctx)
	if settingField(t, view2, "mcp.session_timeout_sec").PendingRestart {
		t.Fatal("restart did not activate session configuration")
	}
}

func TestSettingsSecretsNeverEchoAndInvalidPatchAtomic(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	key, header := "private-ai-credential-1987", "private-gateway-credential-5478"
	view, err := app.AdminPatchSettings(ctx, SettingsPatch{Secrets: map[string]string{"ai.api_key_ref": key, "ai.extra_headers_ref": `{"X-API-Key":"` + header + `"}`}})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := app.Store.GetSettings(ctx)
	if stored["ai.extra_headers"] != "" || stored["ai.extra_headers_ref"] == "" {
		t.Fatal("headers not moved to secret storage")
	}
	if !settingField(t, view, "ai.api_key_ref").Registered || settingField(t, view, "ai.api_key_ref").Value != "" {
		t.Fatal("write-only key contract broken")
	}
	legacy, _ := app.SystemSettings(ctx)
	audit, _ := app.Store.SearchAudit(ctx, DefaultUserID, 100)
	for _, output := range []any{view, legacy, stored, audit} {
		raw, _ := json.Marshal(output)
		if strings.Contains(string(raw), key) || strings.Contains(string(raw), header) {
			t.Fatal("credential exposed in settings or audit")
		}
	}
	ref := app.currentAIConfig().APIKeyRef
	if _, err := app.AdminPatchSettings(ctx, SettingsPatch{Secrets: map[string]string{"ai.api_key_ref": ""}}); err != nil {
		t.Fatal(err)
	}
	if app.currentAIConfig().APIKeyRef != ref {
		t.Fatal("blank secret cleared credential")
	}
	for _, values := range []map[string]string{
		{"ai.temperature": "3", "send.max_per_minute": "99"}, {"ai.task_models": "[]"}, {"ai.task_models": `{"compose":{"base_url":"https://secret@host/v1"}}`}, {"ai.base_url": "https://host/v1?api_key=secret"}, {"mcp.endpoint": "/api"}, {"mcp.endpoint": "/metrics"}, {"mcp.policy": `{"unknown":true}`}, {"storage.data_dir": "/tmp/elsewhere"},
	} {
		if _, err := app.AdminPatchSettings(ctx, SettingsPatch{Values: values}); err == nil {
			t.Fatalf("invalid settings accepted: %+v", values)
		}
	}
	if app.EffectiveConfig().Send.MaxPerMinute == 99 {
		t.Fatal("invalid patch partially saved")
	}
}

func TestAccountPreferencesOwnershipAndInheritance(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	acc := mustAccount(t, app)
	ctx := settingsAdmin()
	if _, err := app.SavePersonalSettings(ctx, "", SettingsPatch{Values: map[string]string{"compose.tone": "friendly"}}); err != nil {
		t.Fatal(err)
	}
	view, err := app.PersonalSettings(ctx, acc.ID)
	if err != nil || settingField(t, view, "compose.tone").Value != "friendly" {
		t.Fatalf("account did not inherit user: %v", err)
	}
	if _, err := app.SavePersonalSettings(ctx, acc.ID, SettingsPatch{Values: map[string]string{"compose.tone": "formal", "account.sync_minutes": "7"}}); err != nil {
		t.Fatal(err)
	}
	values, _ := app.EffectiveMailPreferences(ctx, acc.ID)
	if values["compose.tone"] != "formal" || values["account.sync_minutes"] != "7" {
		t.Fatal("account override missing")
	}
	other := WithPrincipal(ctx, domain.Principal{UserID: "other-admin", Role: domain.RoleAdmin})
	if _, err := app.PersonalSettings(other, acc.ID); err == nil {
		t.Fatal("admin read another user's account preferences")
	}
	if _, err := app.AdminPatchSettings(ctx, SettingsPatch{Values: map[string]string{"mail.html_enabled": "false"}}); err != nil {
		t.Fatal(err)
	}
	view, _ = app.PersonalSettings(ctx, acc.ID)
	if field := settingField(t, view, "compose.format"); field.Value != "text" || !field.Locked {
		t.Fatal("HTML policy bypassed")
	}
}

func TestFixedApprovalPolicyCannotBeDisabled(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	ctx := settingsAdmin()
	account := mustAccount(t, app)
	// Historical settings must never turn an invariant into an editable policy,
	// including after restart. Preserve the historical row without trusting it.
	if err := app.Store.UpsertSettings(ctx, map[string]string{"send.external_approval": "false"}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(app.initialConfig, app.Store, app.Objects, app.Secrets, app.POP3, app.SMTP, app.aiRaw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Shutdown)
	view, err := restarted.AdminSettingsCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	field := settingField(t, view, "send.external_approval")
	if field.Value != "true" || field.Default != "true" || field.Apply != "fixed" || field.Source != "fixed_policy" || !field.Locked || field.Lockable {
		t.Fatalf("fixed policy must not advertise an override: %+v", field)
	}
	if !restarted.SettingBool("send.external_approval") {
		t.Fatal("historical false value overrode the fixed policy")
	}
	legacy, err := restarted.SystemSettings(ctx)
	if err != nil || legacy["send.external_approval"] != "true" {
		t.Fatalf("legacy read advertised disabled approval: %v", err)
	}
	for _, value := range []string{"true", "false"} {
		if _, err := restarted.AdminPatchSettings(ctx, SettingsPatch{Values: map[string]string{"send.external_approval": value, "send.max_per_minute": "99"}}); err == nil {
			t.Fatal("fixed policy write was accepted")
		}
		if err := restarted.AdminSaveSettings(ctx, map[string]string{"send.external_approval": value}, ""); err == nil {
			t.Fatal("legacy settings write accepted a fixed policy")
		}
	}
	if _, err := restarted.AdminPatchSettings(ctx, SettingsPatch{Locks: map[string]bool{"send.external_approval": false}}); err == nil {
		t.Fatal("fixed policy was unlocked")
	}
	if restarted.EffectiveConfig().Send.MaxPerMinute == 99 {
		t.Fatal("rejected fixed policy patch partially applied")
	}
	draft, err := restarted.CreateDraft(ctx, CreateDraftInput{AccountID: account.ID, To: []string{"outside@example.net"}, Subject: "Approval remains required", Body: "Message body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Send(ctx, SendInput{DraftID: draft.Draft.ID}); err == nil || len(smtp.sent) != 0 {
		t.Fatal("historical false approval setting allowed an unapproved send")
	}
}
