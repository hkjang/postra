package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/handoff"
)

// A claim hands over the message once, as markdown, and never again.
func TestHandoffClaimIsSingleUse(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_h1", UserID: DefaultUserID, AccountID: "acc_1", UIDL: "u-h1",
		Subject: "2026년 3분기 개편안", From: domain.Address{Name: "Alice", Email: "alice@example.com"},
		To: []domain.Address{{Email: "me@corp.local"}}, RawHash: "h-h1", RawURI: "mem://h1", Date: now, CreatedAt: now,
	}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID, TextBody: "첫 줄\r\n둘째 줄\r\n"},
		[]domain.Attachment{{ID: "att_1", MessageID: m.ID, Name: "plan.xlsx", MIMEType: "application/vnd.ms-excel", Size: 2048}}); err != nil {
		t.Fatal(err)
	}

	view, err := app.IssueHandoffClaim(ctx, "msg_h1", "markdown", "https://postra.intra")
	if err != nil {
		t.Fatal(err)
	}
	if !handoff.ValidClaimToken(view.Claim) || view.Source != "https://postra.intra" ||
		view.Filename != "2026년 3분기 개편안.md" || view.ContentType != handoff.ContentType || view.Bytes <= 0 {
		t.Fatalf("unexpected claim: %+v", view)
	}
	expires, err := time.Parse(time.RFC3339, view.ExpiresAt)
	if err != nil || time.Until(expires) > handoff.ClaimTTL || time.Until(expires) < handoff.ClaimTTL-time.Minute {
		t.Fatalf("expires_at should be about five minutes out: %s (%v)", view.ExpiresAt, err)
	}

	doc, err := app.CollectHandoffClaim(context.Background(), view.Claim)
	if err != nil {
		t.Fatal(err)
	}
	body := string(doc.Body)
	if int64(len(doc.Body)) != view.Bytes || doc.Filename != view.Filename || doc.ContentType != handoff.ContentType {
		t.Fatalf("collected document does not match the claim: %+v vs %+v", doc, view)
	}
	for _, want := range []string{"# 2026년 3분기 개편안\n", "- **보낸이**: Alice <alice@example.com>\n", "- **받는이**: me@corp.local\n",
		"- **첨부**: plan.xlsx (2 KB)\n", "\n---\n\n첫 줄\n둘째 줄\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("markdown missing %q:\n%s", want, body)
		}
	}

	// Second collection: 404, indistinguishable from a claim that never was.
	if _, err := app.CollectHandoffClaim(context.Background(), view.Claim); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second collection should be not found, got %v", err)
	}
	other, _, _ := handoff.NewClaim()
	if _, err := app.CollectHandoffClaim(context.Background(), other); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown claim should be not found, got %v", err)
	}
	if _, err := app.CollectHandoffClaim(context.Background(), "not-a-claim"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("malformed claim should be not found, got %v", err)
	}

	// The token is never written down: not in the audit trail, only its digest in the store.
	events, err := app.SearchAudit(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var issued, collected bool
	for _, ev := range events {
		if strings.Contains(ev.Detail, view.Claim) || strings.Contains(ev.Resource, view.Claim) {
			t.Fatalf("audit event carries the claim token: %+v", ev)
		}
		issued = issued || ev.Action == "handoff_claim_issued"
		collected = collected || ev.Action == "handoff_claim_collected"
	}
	if !issued || !collected {
		t.Fatalf("issue and collection should be audited: issued=%v collected=%v", issued, collected)
	}
}

func TestHandoffClaimExpires(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	recentMessage(t, app, ctx, "acc_1", "msg_h2", "old", "body")
	token, digest, _ := handoff.NewClaim()
	if err := app.Store.InsertHandoffClaim(ctx, &domain.HandoffClaim{
		Digest: digest, UserID: DefaultUserID, MessageID: "msg_h2", Filename: "old.md",
		ContentType: handoff.ContentType, Bytes: 1, ExpiresAt: time.Now().Add(-time.Second).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.CollectHandoffClaim(ctx, token); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired claim should be not found, got %v", err)
	}
	// Issuing a new claim sweeps the expired row.
	if _, err := app.IssueHandoffClaim(ctx, "msg_h2", "markdown", "http://localhost:8480"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.ConsumeHandoffClaim(ctx, digest, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired row should have been swept, got %v", err)
	}
}

// A claim is bound to a message the caller can read; nobody else's mail.
func TestHandoffClaimRefusesOtherUsersMessage(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	recentMessage(t, app, ctx, "acc_1", "msg_h3", "mine", "body")
	if err := app.Store.EnsureUser(ctx, "usr_other", "other"); err != nil {
		t.Fatal(err)
	}
	otherCtx := WithPrincipal(ctx, domain.Principal{UserID: "usr_other", LoginID: "other", Role: domain.RoleUser})
	if _, err := app.IssueHandoffClaim(otherCtx, "msg_h3", "markdown", "https://postra.intra"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another user's message must not yield a claim, got %v", err)
	}
	if _, err := app.IssueHandoffClaim(ctx, "msg_h3", "docx", "https://postra.intra"); err == nil {
		t.Fatal("this service sends markdown only")
	}
	if _, err := app.IssueHandoffClaim(ctx, "msg_h3", "markdown", ""); err == nil {
		t.Fatal("without a public origin there is no source to announce")
	}
}

// Collection happens on a public route with no principal in the context, so
// the event must be attributed to the user who issued the claim — the owner
// of the message — not to whoever the context defaults to.
func TestHandoffCollectionIsAuditedToClaimOwner(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	if err := app.Store.EnsureUser(ctx, "usr_other", "other"); err != nil {
		t.Fatal(err)
	}
	otherCtx := WithPrincipal(ctx, domain.Principal{UserID: "usr_other", LoginID: "other", Role: domain.RoleUser})
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_h5", UserID: "usr_other", AccountID: "acc_other", UIDL: "u-h5",
		Subject: "theirs", From: domain.Address{Email: "sender@example.com"},
		RawHash: "h-h5", RawURI: "mem://h5", Date: now, CreatedAt: now,
	}
	if err := app.Store.InsertMessage(otherCtx, m, &domain.MessageBody{MessageID: m.ID, TextBody: "body"}, nil); err != nil {
		t.Fatal(err)
	}
	view, err := app.IssueHandoffClaim(otherCtx, "msg_h5", "markdown", "https://postra.intra")
	if err != nil {
		t.Fatal(err)
	}
	// The receiving service collects without logging in: no principal at all.
	if _, err := app.CollectHandoffClaim(WithActor(context.Background(), "rest"), view.Claim); err != nil {
		t.Fatal(err)
	}

	collectedFor := func(c context.Context) bool {
		events, err := app.SearchAudit(c, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if ev.Action == "handoff_claim_collected" && ev.Resource == "message:msg_h5" {
				return true
			}
		}
		return false
	}
	if !collectedFor(otherCtx) {
		t.Fatal("collection should appear in the claim owner's audit log")
	}
	if collectedFor(ctx) {
		t.Fatal("collection must not be attributed to the default user")
	}
}

// The announced source is the pinned public origin when there is one, else
// the address the request came in on.
func TestHandoffSourceOrigin(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	recentMessage(t, app, ctx, "acc_1", "msg_h4", "s", "b")
	if err := app.Store.UpsertSettings(ctx, map[string]string{handoff.SettingSourceOrigin: "https://Postra.Intra/"}); err != nil {
		t.Fatal(err)
	}
	view, err := app.IssueHandoffClaim(ctx, "msg_h4", "markdown", "http://10.0.0.5:8480")
	if err != nil {
		t.Fatal(err)
	}
	if view.Source != "https://postra.intra" {
		t.Fatalf("source = %q, want the pinned origin", view.Source)
	}
}

// The allow list is empty until an administrator fills it, only services that
// read markdown are offered, and a save that would break the list is refused.
func TestHandoffTargetsSettings(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	if targets := app.HandoffTargets(ctx); len(targets) != 0 {
		t.Fatalf("fresh installation must have no targets: %+v", targets)
	}
	settings, err := app.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := settings[handoff.SettingTargets]; !ok || v != "" {
		t.Fatalf("targets default should be present and empty: %q ok=%v", v, ok)
	}
	admin := WithPrincipal(ctx, domain.Principal{UserID: DefaultUserID, Role: domain.RoleAdmin})
	err = app.AdminSaveSettings(admin, map[string]string{handoff.SettingTargets: `[{"name":"A","origin":"https://a.intra/x","formats":["markdown"]}]`}, "")
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("origin with a path should be refused as a user error, got %v", err)
	}
	if err := app.AdminSaveSettings(admin, map[string]string{handoff.SettingSourceOrigin: "postra.intra"}, ""); err == nil {
		t.Fatal("a source origin without a scheme should be refused")
	}
	if err := app.AdminSaveSettings(admin, map[string]string{handoff.SettingTargets: `[
	  {"name":"Ptium","origin":"https://ptium.intra","formats":["markdown","docx"]},
	  {"name":"Kanpic","origin":"https://kanpic.intra","formats":["csv","xlsx"]}]`}, ""); err != nil {
		t.Fatal(err)
	}
	targets := app.HandoffTargets(ctx)
	if len(targets) != 1 || targets[0].Name != "Ptium" {
		t.Fatalf("only markdown readers are offered: %+v", targets)
	}
}
