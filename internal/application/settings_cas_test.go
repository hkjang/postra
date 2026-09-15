package application

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

type settingsCASAttempt struct{ expected, updates map[string]string }
type gatedSettingsStore struct {
	Storage
	cas     domain.AdminSettingsCompareAndSwapper
	ready   chan<- settingsCASAttempt
	release <-chan struct{}
}

func (s gatedSettingsStore) CompareAndSwapAdminSettings(ctx context.Context, expected, updates map[string]string) (bool, error) {
	select {
	case s.ready <- settingsCASAttempt{expected, updates}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return s.cas.CompareAndSwapAdminSettings(ctx, expected, updates)
}

func TestAdminSettingsCASAcrossAppsAndLegacyWriters(t *testing.T) {
	for _, kind := range []string{"patch", "legacy", "ai", "tracking"} {
		t.Run(kind, func(t *testing.T) {
			first, _, _, _ := newTestApp(t)
			secondStore, err := persistence.Open(filepath.Join(first.Cfg.DataDir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer secondStore.Close()
			second, err := New(first.initialConfig, secondStore, first.Objects, first.Secrets, first.POP3, first.SMTP, first.aiRaw)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Shutdown()
			ctx, cancel := context.WithTimeout(settingsAdmin(), 10*time.Second)
			defer cancel()
			view, err := first.AdminSettingsCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ready, release := make(chan settingsCASAttempt, 2), make(chan struct{})
			first.Store = gatedSettingsStore{first.Store, first.Store.(domain.AdminSettingsCompareAndSwapper), ready, release}
			second.Store = gatedSettingsStore{secondStore, secondStore, ready, release}
			errs := make(chan error, 2)
			for index, app := range []*App{first, second} {
				go func(index int, app *App) {
					var err error
					switch kind {
					case "patch":
						_, err = app.AdminPatchSettings(ctx, SettingsPatch{Revision: view.Revision, Values: map[string]string{"compose.template": []string{"formal", "notice"}[index]}, Secrets: map[string]string{SettingAIAPIKeyRef: []string{"ephemeral-key-A", "ephemeral-key-B"}[index]}})
					case "legacy":
						err = app.AdminSaveSettings(ctx, map[string]string{SettingSyncAutoMinutes: []string{"3", "5"}[index]}, "ephemeral-oidc-secret")
					case "ai":
						err = app.AdminSaveAISettings(ctx, map[string]string{SettingAIBaseURL: "http://127.0.0.1:11434/v1", SettingAIModel: []string{"model-a", "model-b"}[index]}, "ephemeral-ai-secret")
					case "tracking":
						err = app.AdminAllowTrackingHost(ctx, []string{"https://first.corp.local", "https://second.corp.local"}[index])
					}
					errs <- err
				}(index, app)
			}
			attempts := []settingsCASAttempt{}
			for range 2 {
				select {
				case attempt := <-ready:
					attempts = append(attempts, attempt)
				case <-ctx.Done():
					close(release)
					t.Fatal("both independent instances did not reach CAS")
				}
			}
			close(release)
			wins, conflicts := 0, 0
			for range 2 {
				err := <-errs
				if err == nil {
					wins++
					continue
				}
				var public *domain.PublicError
				if !errors.As(err, &public) || public.Status != 409 || public.Code != "conflict" {
					t.Fatalf("wrong concurrency outcome: %v", err)
				}
				conflicts++
			}
			if wins != 1 || conflicts != 1 {
				t.Fatalf("lost-update outcome wins=%d conflicts=%d", wins, conflicts)
			}
			stored, _ := secondStore.GetSettings(ctx)
			for _, attempt := range attempts {
				for _, key := range []string{SettingAIAPIKeyRef, SettingOIDCSecretRef} {
					ref := attempt.updates[key]
					if ref == "" {
						continue
					}
					handle, err := first.Secrets.Acquire(ctx, domain.SecretRef(ref), domain.PurposeTest)
					if stored[key] == ref {
						if err != nil {
							t.Fatal("winner's secret was revoked")
						}
						handle.Zero()
					} else if err == nil {
						handle.Zero()
						t.Fatal("loser's uncommitted secret remained usable")
					}
				}
			}
		})
	}
}

func TestPersonalSettingsRevisionsBindUserAndAccount(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	first, second := mustAccount(t, app), mustAccount(t, app)
	viewA, err := app.PersonalSettings(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	viewB, err := app.PersonalSettings(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if viewA.Revision == viewB.Revision {
		t.Fatal("identical fields on different accounts share a revision")
	}
	if _, err := app.SavePersonalSettings(ctx, second.ID, SettingsPatch{Revision: viewA.Revision, Values: map[string]string{"compose.template": "formal"}}); err == nil {
		t.Fatal("different-account revision accepted")
	}
	if err := app.Store.EnsureUser(ctx, "revision-other", "revision-other"); err != nil {
		t.Fatal(err)
	}
	other := WithPrincipal(context.Background(), domain.Principal{UserID: "revision-other", Role: domain.RoleUser})
	ownerView, _ := app.PersonalSettings(ctx, "")
	otherView, _ := app.PersonalSettings(other, "")
	if ownerView.Revision == otherView.Revision {
		t.Fatal("different users share a revision")
	}
	if _, err := app.SavePersonalSettings(other, "", SettingsPatch{Revision: ownerView.Revision, Values: map[string]string{"compose.template": "formal"}}); err == nil {
		t.Fatal("different-user revision accepted")
	}
}

type settingsStoreWithoutCAS struct{ Storage }

func TestAdminSettingsFailClosedWithoutAtomicStoreSupport(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	store := app.Store
	app.Store = settingsStoreWithoutCAS{store}
	err := app.AdminSaveSettings(settingsAdmin(), map[string]string{SettingSyncAutoMinutes: "9"}, "")
	var public *domain.PublicError
	if !errors.As(err, &public) || public.Status != 503 {
		t.Fatalf("unsafe custom-store fallback: %v", err)
	}
	stored, _ := store.GetSettings(context.Background())
	if stored[SettingSyncAutoMinutes] == "9" {
		t.Fatal("unsupported store changed settings")
	}
}

// A driver can lose the transaction completion response after the database has
// durably committed. Cleanup must never turn this uncertain response into a
// definitively broken, persisted credential reference.
type lostCommitResponseSettingsStore struct {
	Storage
	cas domain.AdminSettingsCompareAndSwapper
}

func (s lostCommitResponseSettingsStore) CompareAndSwapAdminSettings(ctx context.Context, expected, updates map[string]string) (bool, error) {
	committed, err := s.cas.CompareAndSwapAdminSettings(ctx, expected, updates)
	if err != nil || !committed {
		return committed, err
	}
	return false, errors.New("fixture: commit response lost")
}

func TestAdminSettingsCASLostCommitResponsePreservesPersistedSecret(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	store := app.Store
	app.Store = lostCommitResponseSettingsStore{store, store.(domain.AdminSettingsCompareAndSwapper)}
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Secrets: map[string]string{SettingAIAPIKeyRef: "ephemeral-commit-fixture"}}); err == nil {
		t.Fatal("lost completion response unexpectedly reported success")
	}
	stored, err := store.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stored[SettingAIAPIKeyRef] == "" {
		t.Fatal("fixture did not commit the reference")
	}
	handle, err := app.Secrets.Acquire(context.Background(), domain.SecretRef(stored[SettingAIAPIKeyRef]), domain.PurposeTest)
	if err != nil {
		t.Fatal("persisted secret was revoked after uncertain COMMIT response")
	}
	handle.Zero()
}

func TestAdminSettingsCASRotationPreservesSharedSettingAndAccountSecret(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	handle := domain.NewSecretHandle([]byte("ephemeral-shared-setting-fixture"))
	ref, err := app.RegisterSecret(ctx, domain.SecretAPIKey, "shared fixture", handle)
	handle.Zero()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AdminSaveAISettings(ctx, map[string]string{SettingAIBaseURL: "http://127.0.0.1:11434/v1", SettingAIModel: "fixture", SettingAIAPIKeyRef: string(ref)}, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AdminSaveSettings(ctx, map[string]string{SettingOIDCSecretRef: string(ref)}, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AdminSaveAISettings(ctx, map[string]string{SettingAIBaseURL: "http://127.0.0.1:11434/v1", SettingAIModel: "fixture"}, "ephemeral-rotated-fixture"); err != nil {
		t.Fatal(err)
	}
	assertUsable := func(reason string) {
		t.Helper()
		handle, err := app.Secrets.Acquire(ctx, ref, domain.PurposeTest)
		if err != nil {
			t.Fatal(reason)
		}
		handle.Zero()
	}
	assertUsable("rotation revoked a reference still used by OIDC")
	account := mustAccount(t, app)
	account.POP3Secret, account.SMTPSecret = ref, ref
	if err := app.Store.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := app.AdminSaveSettings(ctx, map[string]string{SettingOIDCSecretRef: ""}, ""); err != nil {
		t.Fatal(err)
	}
	assertUsable("OIDC removal revoked a reference still used by an account")
}
