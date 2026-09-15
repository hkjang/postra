package application

import (
	"context"
	"testing"
	"time"

	"postra/internal/domain"
)

func TestWorkFiveStatesAndLegacyAliasFilters(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	account := mustAccount(t, app)
	for _, status := range []string{"open", "new", "needs_action", "pending", "in_progress", "waiting", "resolved", "done"} {
		message := recentMessage(t, app, ctx, account.ID, "work-"+status, status, "body")
		result, err := app.SetMessageWorkStatus(ctx, message.ID, status)
		if err != nil || result.Status != status {
			t.Fatalf("status/legacy write %s: %+v %v", status, result, err)
		}
	}
	for _, test := range []struct {
		filter string
		count  int
	}{{"new", 2}, {"open", 2}, {"needs_action", 1}, {"pending", 2}, {"in_progress", 2}, {"waiting", 1}, {"resolved", 2}, {"done", 2}} {
		items, err := app.TeamInbox(ctx, test.filter, "", 100)
		if err != nil || len(items) != test.count {
			t.Fatalf("alias filter %s: %d %v", test.filter, len(items), err)
		}
	}
	if _, err := app.SetMessageWorkStatus(ctx, "work-open", "closed-invalid"); err == nil {
		t.Fatal("invalid state accepted")
	}
	if _, err := app.TeamInbox(ctx, "invalid", "", 100); err == nil {
		t.Fatal("invalid filter accepted")
	}
	if _, err := app.SetMessageSLA(ctx, "work-new", -1); err == nil {
		t.Fatal("negative SLA accepted")
	}
	due := time.Now().Add(time.Hour).Unix()
	if result, err := app.SetMessageSLA(ctx, "work-new", due); err != nil || result.SLADue != due {
		t.Fatalf("SLA failed: %+v %v", result, err)
	}
	other := WithPrincipal(context.Background(), domain.Principal{UserID: "other", Role: domain.RoleAdmin})
	if _, err := app.SetMessageWorkStatus(other, "work-new", "done"); err == nil {
		t.Fatal("administrator changed another owner's mail work")
	}
}

func TestManualActionCreationValidationOwnershipAndMCPScopes(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	account := mustAccount(t, app)
	message := recentMessage(t, app, ctx, account.ID, "msg_manual_action", "action", "private mail")
	input := CreateActionCardInput{MessageID: message.ID, Title: "직접 만든 할 일", Due: "2026-09-20", Assignee: "hong"}
	card, err := app.CreateActionCard(ctx, input)
	if err != nil || card.Status != "pending" || card.Type != "todo" || card.Confidence != 0 {
		t.Fatalf("manual action: %+v %v", card, err)
	}
	for _, bad := range []CreateActionCardInput{{MessageID: message.ID}, {Title: "no source"}, {MessageID: message.ID, Title: "bad due", Due: "next Friday"}, {MessageID: message.ID, Title: "invalid calendar", Due: "2026-02-30"}} {
		if _, err := app.CreateActionCard(ctx, bad); err == nil {
			t.Fatalf("invalid action accepted: %+v", bad)
		}
	}
	other := WithPrincipal(context.Background(), domain.Principal{UserID: "other", Role: domain.RoleAdmin})
	if _, err := app.CreateActionCard(other, input); err == nil {
		t.Fatal("administrator created action on another user's mail")
	}
	key := domain.Principal{UserID: DefaultUserID, Role: domain.RoleUser, AuthMethod: "mcp_key", MCPKeyID: "work-test", MCPScopes: []string{"mail.read"}}
	if _, err := app.CreateActionCard(WithPrincipal(context.Background(), key), input); err == nil {
		t.Fatal("read key created action")
	}
	key.MCPScopes = []string{"mail.work"}
	if _, err := app.CreateActionCard(WithPrincipal(context.Background(), key), input); err != nil {
		t.Fatal(err)
	}
}
