package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"postra/internal/domain"
)

func TestNotificationEventsOwnershipPreferencesAndNoSensitivePayload(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	const marker = "never-expose-mail-body-api-key-password"
	for _, owner := range []string{DefaultUserID, "other-notification-user"} {
		if err := app.Store.EnsureUser(ctx, owner, owner); err != nil {
			t.Fatal(err)
		}
		if err := app.Store.CreateJob(ctx, &domain.Job{ID: "job_" + owner, UserID: owner, Type: "sync", Status: domain.JobRunning, Progress: marker, Error: marker, Meta: map[string]string{"secret": marker}}); err != nil {
			t.Fatal(err)
		}
		if err := app.Store.CreateJob(ctx, &domain.Job{ID: "embed_" + owner, UserID: owner, Type: "embed", Status: domain.JobSucceeded, Progress: marker}); err != nil {
			t.Fatal(err)
		}
		if err := app.Store.CreateActionCard(ctx, &domain.ActionCard{ID: "card_" + owner, UserID: owner, MessageID: "message", Title: marker, Detail: marker, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
		if err := app.Store.CreateOutbound(ctx, &domain.OutboundMessage{ID: "out_" + owner, UserID: owner, DraftID: "draft", IdempotencyKey: marker + owner, Status: domain.OutboundRetryWait, SMTPResponse: marker}); err != nil {
			t.Fatal(err)
		}
		if err := app.Store.AppendAudit(ctx, domain.AuditEvent{UserID: owner, Actor: "test", Action: "login", Resource: marker, Detail: marker, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := app.NotificationEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Events) != 5 || !snapshot.Enabled || snapshot.UserID != DefaultUserID {
		t.Fatalf("unexpected snapshot %+v", snapshot)
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), marker) || strings.Contains(string(encoded), "other-notification-user") {
		t.Fatalf("sensitive event payload: %s", encoded)
	}
	if _, err := app.SavePersonalSettings(ctx, "", SettingsPatch{Values: map[string]string{"notifications.sync": "false", "notifications.ai": "false", "notifications.send": "false", "notifications.action": "false"}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = app.NotificationEvents(ctx)
	if err != nil || len(snapshot.Events) != 1 || snapshot.Events[0].Category != "security" {
		t.Fatalf("personal category flags ignored: %+v %v", snapshot, err)
	}
	if _, err := app.AdminPatchSettings(ctx, SettingsPatch{Values: map[string]string{"notifications.enabled": "false", "notifications.poll_seconds": "9"}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = app.NotificationEvents(ctx)
	if err != nil || snapshot.Enabled || len(snapshot.Events) != 0 || snapshot.PollSeconds != 9 {
		t.Fatalf("global disable ignored: %+v %v", snapshot, err)
	}
	revision := snapshot.PreferencesRevision
	if revision == "" {
		t.Fatal("disabled notifications hid preference revision")
	}
	if _, err := app.AdminPatchSettings(ctx, SettingsPatch{Values: map[string]string{"ui.theme": "dark"}, Locks: map[string]bool{"ui.theme": true}}); err != nil {
		t.Fatal(err)
	}
	if updated, err := app.NotificationEvents(ctx); err != nil || updated.PreferencesRevision == revision || len(updated.Events) != 0 {
		t.Fatalf("disabled stream missed policy revision: %+v %v", updated, err)
	}
	if _, err := app.NotificationEvents(context.Background()); err == nil {
		t.Fatal("anonymous metadata access accepted")
	}
	keyCtx := WithPrincipal(context.Background(), domain.Principal{UserID: DefaultUserID, Role: domain.RoleUser, AuthMethod: "mcp_key", MCPKeyID: "fixture", MCPScopes: []string{}})
	if _, err := app.NotificationEvents(keyCtx); err == nil {
		t.Fatal("empty-scope key read notifications")
	}
}
