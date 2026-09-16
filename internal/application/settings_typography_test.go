package application

import (
	"context"
	"testing"

	"postra/internal/domain"
)

func TestTextSizePreferencePersistenceAndPolicy(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	admin := settingsAdmin()
	if err := app.Store.EnsureUser(admin, "text-size-user", "text-size-user"); err != nil {
		t.Fatal(err)
	}
	user := WithPrincipal(context.Background(), domain.Principal{UserID: "text-size-user", Role: domain.RoleUser})
	view, err := app.PersonalSettings(user, "")
	if err != nil {
		t.Fatal(err)
	}
	field := settingField(t, view, "ui.text_size")
	if field.Value != "standard" || field.Source != "default" || field.Apply != "live" {
		t.Fatalf("unexpected default: %+v", field)
	}
	if _, err := app.SavePersonalSettings(user, "", SettingsPatch{Values: map[string]string{"ui.text_size": "huge"}}); err == nil {
		t.Fatal("invalid font size accepted")
	}
	view, err = app.SavePersonalSettings(user, "", SettingsPatch{Values: map[string]string{"ui.text_size": "large"}})
	if err != nil {
		t.Fatal(err)
	}
	if field = settingField(t, view, "ui.text_size"); field.Value != "large" || field.Source != "user" {
		t.Fatalf("personal preference missing: %+v", field)
	}
	other, err := app.PersonalSettings(admin, "")
	if err != nil || settingField(t, other, "ui.text_size").Value != "standard" {
		t.Fatalf("preference leaked across identities: %v", err)
	}
	app2, err := New(app.initialConfig, app.Store, app.Objects, app.Secrets, app.POP3, app.SMTP, app.aiRaw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app2.Shutdown)
	view, err = app2.PersonalSettings(user, "")
	if err != nil || settingField(t, view, "ui.text_size").Value != "large" {
		t.Fatalf("preference lost on restart: %v", err)
	}
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{"ui.text_size": "standard"}, Locks: map[string]bool{"ui.text_size": true}}); err != nil {
		t.Fatal(err)
	}
	view, err = app.PersonalSettings(user, "")
	if err != nil {
		t.Fatal(err)
	}
	if field = settingField(t, view, "ui.text_size"); field.Value != "standard" || !field.Locked {
		t.Fatalf("admin policy ignored: %+v", field)
	}
	if _, err := app.SavePersonalSettings(user, "", SettingsPatch{Values: map[string]string{"ui.text_size": "large"}}); err == nil {
		t.Fatal("locked preference was editable")
	}
}
