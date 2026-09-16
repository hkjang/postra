package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/notifymail"
)

// notifyMailHarness switches the relay on with a fake SMTP and two users:
// an admin (the default principal) and "hong", a plain user with an address.
func notifyMailHarness(t *testing.T) (*App, *fakeSMTP, context.Context, *domain.User) {
	t.Helper()
	app, _, smtp, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	admin := &domain.User{ID: DefaultUserID}
	if u, err := app.Store.GetUser(ctx, DefaultUserID); err == nil {
		admin = u
	}
	admin.Email, admin.Role, admin.Status, admin.LoginID = "admin@corp.local", domain.RoleAdmin, domain.UserActive, "admin"
	if err := app.Store.UpdateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	hong := &domain.User{ID: persistence.NewID("usr"), LoginID: "hong", DisplayName: "홍길동", Email: "hong@corp.local",
		Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "local"}
	if err := app.Store.CreateUser(ctx, hong, ""); err != nil {
		t.Fatal(err)
	}
	adminCtx := WithPrincipal(ctx, domain.Principal{UserID: DefaultUserID, LoginID: "admin", Role: domain.RoleAdmin})
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{
		notifymail.KeyEnabled: "true", notifymail.KeyHost: "relay.corp.local", notifymail.KeyFromAddress: "noreply@corp.local",
		notifymail.KeyBaseURL: "https://postra.corp.local",
	}}); err != nil {
		t.Fatal(err)
	}
	return app, smtp, adminCtx, hong
}

func findDelivery(list []domain.MailDelivery, subject string) *domain.MailDelivery {
	for i := range list {
		if strings.Contains(list[i].Subject, subject) {
			return &list[i]
		}
	}
	return nil
}

func waitNotifyMail(t *testing.T, app *App) []domain.MailDelivery {
	t.Helper()
	app.workerGroup.Wait()
	list, err := app.Store.ListMailDeliveries(context.Background(), domain.MailDeliveryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestNotifyMailOffByDefaultAndSilentWhenDisabled(t *testing.T) {
	app, smtp, _, hong := newTestAppWithUser(t)
	cfg := app.NotifyMailConfig()
	if cfg.Enabled || cfg.Port != 25 || cfg.Security != "auto" || cfg.Username != "" {
		t.Fatalf("fresh install must be off with relay defaults: %+v", cfg)
	}
	app.NotifyMail(context.Background(), notifymail.Assigned("admin", "s", "m"), "", []string{hong.ID})
	if list := waitNotifyMail(t, app); len(list) != 0 || len(smtp.sent) != 0 {
		t.Fatalf("disabled relay must send and record nothing: %d/%d", len(list), len(smtp.sent))
	}
}

func newTestAppWithUser(t *testing.T) (*App, *fakeSMTP, context.Context, *domain.User) {
	t.Helper()
	app, _, smtp, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	hong := &domain.User{ID: persistence.NewID("usr"), LoginID: "hong", Email: "hong@corp.local", Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "local"}
	if err := app.Store.CreateUser(ctx, hong, ""); err != nil {
		t.Fatal(err)
	}
	return app, smtp, ctx, hong
}

func TestNotifyMailEnabledWithoutHostLeavesAReason(t *testing.T) {
	app, smtp, ctx, hong := newTestAppWithUser(t)
	adminCtx := WithPrincipal(ctx, domain.Principal{UserID: DefaultUserID, Role: domain.RoleAdmin})
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.KeyEnabled: "true"}}); err != nil {
		t.Fatal(err)
	}
	app.NotifyMail(ctx, notifymail.Assigned("admin", "s", "m"), "", []string{hong.ID})
	if list := waitNotifyMail(t, app); len(list) != 0 || len(smtp.sent) != 0 {
		t.Fatal("half-configured relay must not send")
	}
	incidents, _ := app.Store.ListIncidents(ctx, domain.IncidentFilter{Component: "notify-mail"})
	if len(incidents) != 1 || !strings.Contains(incidents[0].Message, notifymail.KeyHost) {
		t.Fatalf("expected a warning incident naming the missing host, got %+v", incidents)
	}
}

func TestNotifyMailSendsInBackgroundAndRecordsBothOutcomes(t *testing.T) {
	app, smtp, _, hong := notifyMailHarness(t)
	ctx := context.Background()
	app.NotifyMail(ctx, notifymail.Assigned("admin", "견적 요청", "msg_1"), "", []string{hong.LoginID})
	list := waitNotifyMail(t, app)
	if len(list) != 1 || list[0].Status != domain.MailDeliverySent || list[0].Recipient != "hong@corp.local" || list[0].Attempts != 1 {
		t.Fatalf("expected one sent delivery to hong, got %+v", list)
	}
	if len(smtp.sent) != 1 || smtp.sent[0].env.From != "noreply@corp.local" || smtp.sent[0].env.To[0] != "hong@corp.local" {
		t.Fatalf("relay envelope wrong: %+v", smtp.sent)
	}
	raw := string(smtp.sent[0].raw)
	if !strings.Contains(raw, "https://postra.corp.local/app/mail/msg_1") || !strings.Contains(raw, "Auto-Submitted: auto-generated") || !strings.Contains(raw, "=?utf-8?q?") {
		t.Fatalf("mail body/headers unexpected:\n%s", raw)
	}
	if smtp.lastOpts.Port != 25 || smtp.lastOpts.Security != domain.SecurityNone || !smtp.lastOpts.OpportunisticTLS || smtp.lastOpts.AuthMethod != "none" {
		t.Fatalf("auto mode must dial plaintext on 25 with opportunistic STARTTLS and no auth: %+v", smtp.lastOpts)
	}

	// A dead relay fails the delivery, never the caller, and is still recorded.
	smtp.permFail = true
	app.NotifyMail(ctx, notifymail.Assigned("admin", "두 번째", "msg_2"), "", []string{hong.ID})
	list = waitNotifyMail(t, app)
	failed := findDelivery(list, "두 번째")
	if len(list) != 2 || failed == nil || failed.Status != domain.MailDeliveryFailed || failed.Attempts != 2 || failed.Error == "" {
		t.Fatalf("expected a failed delivery with two attempts, got %+v", list)
	}
	page, err := app.AdminListMailDeliveries(WithPrincipal(ctx, domain.Principal{UserID: DefaultUserID, Role: domain.RoleAdmin}), "failed", 10)
	if err != nil || page.Summary.Total != 2 || page.Summary.Status["sent"] != 1 || len(page.Items) != 1 {
		t.Fatalf("admin listing wrong: %+v %v", page, err)
	}
	if _, err := app.AdminListMailDeliveries(WithPrincipal(ctx, domain.Principal{UserID: hong.ID, Role: domain.RoleUser}), "", 10); err == nil {
		t.Fatal("non-admin must not read the delivery log")
	}
}

func TestNotifyMailSkipsActorAndHonoursSwitches(t *testing.T) {
	app, smtp, adminCtx, hong := notifyMailHarness(t)
	ctx := context.Background()
	// The actor is never told about their own action, and duplicates collapse.
	app.NotifyMail(ctx, notifymail.Assigned("hong", "s", "m"), hong.ID, []string{hong.ID, hong.LoginID, DefaultUserID, DefaultUserID})
	list := waitNotifyMail(t, app)
	if len(list) != 1 || list[0].Recipient != "admin@corp.local" {
		t.Fatalf("expected only the admin to be mailed, got %+v", list)
	}
	// Per-event switch stops that kind only.
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.NotifyKey(notifymail.EventAssigned): "false"}}); err != nil {
		t.Fatal(err)
	}
	app.NotifyMail(ctx, notifymail.Assigned("admin", "s", "m"), "", []string{hong.ID})
	app.NotifyMail(ctx, notifymail.SendFailed("s", "why"), "", []string{hong.ID})
	list = waitNotifyMail(t, app)
	if len(list) != 2 || findDelivery(list, "멈췄습니다") == nil {
		t.Fatalf("assigned must be muted while send_failed still goes out: %+v", list)
	}
	// The receiver decides: hong turned action mails off personally.
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.NotifyKey(notifymail.EventAssigned): "true"}}); err != nil {
		t.Fatal(err)
	}
	hongCtx := WithPrincipal(ctx, domain.Principal{UserID: hong.ID, LoginID: hong.LoginID, Role: domain.RoleUser})
	if _, err := app.SavePersonalSettings(hongCtx, "", SettingsPatch{Values: map[string]string{"notifications.action": "false"}}); err != nil {
		t.Fatal(err)
	}
	app.NotifyMail(ctx, notifymail.Assigned("admin", "s", "m"), "", []string{hong.ID})
	if list = waitNotifyMail(t, app); len(list) != 2 {
		t.Fatalf("personal opt-out ignored: %+v", list)
	}
	// Master switch off: nothing at all, even for allowed events.
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.KeyEnabled: "false"}}); err != nil {
		t.Fatal(err)
	}
	app.NotifyMail(ctx, notifymail.SendFailed("s", "why"), "", []string{hong.ID})
	if list = waitNotifyMail(t, app); len(list) != 2 || len(smtp.sent) != 2 {
		t.Fatalf("master switch ignored: %d deliveries, %d sent", len(list), len(smtp.sent))
	}
}

func TestNotifyMailPasswordIsWriteOnly(t *testing.T) {
	app, smtp, adminCtx, _ := notifyMailHarness(t)
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.KeyPassword: "plain"}}); err == nil {
		t.Fatal("password must be rejected as a plain value")
	}
	view, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.KeyUsername: "relayuser"}, Secrets: map[string]string{notifymail.KeyPassword: "hunter2"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range view.Fields {
		if f.Key == notifymail.KeyPassword && (f.Value != "" || !f.Registered || f.Default != "") {
			t.Fatalf("password must read back as registered only: %+v", f)
		}
	}
	stored, _ := app.Store.GetSettings(context.Background())
	if v := stored[notifymail.KeyPassword]; !strings.HasPrefix(v, "sec_") {
		t.Fatalf("settings table must hold a secret reference, got %q", v)
	}
	// The legacy settings endpoint registers a plaintext password the same way.
	if err := app.AdminSaveSettings(adminCtx, map[string]string{notifymail.KeyPassword: "legacy-plain"}, ""); err != nil {
		t.Fatal(err)
	}
	stored, _ = app.Store.GetSettings(context.Background())
	if v := stored[notifymail.KeyPassword]; !strings.HasPrefix(v, "sec_") || strings.Contains(v, "legacy") {
		t.Fatalf("legacy path persisted the plaintext: %q", v)
	}
	if legacy, _ := app.SystemSettings(adminCtx); strings.Contains(legacy[notifymail.KeyPassword], "hunter2") {
		t.Fatal("legacy settings endpoint leaked the password")
	}
	audits, _ := app.Store.SearchAudit(context.Background(), DefaultUserID, 20)
	for _, ev := range audits {
		if strings.Contains(ev.Detail, "hunter2") {
			t.Fatalf("audit trail leaked the password: %s", ev.Detail)
		}
	}
	// The relay session authenticates with the secret, revealed only in the adapter.
	d, err := app.AdminSendTestMail(adminCtx, "")
	if err != nil || d.Status != domain.MailDeliverySent || d.Recipient != "admin@corp.local" {
		t.Fatalf("test send: %+v %v", d, err)
	}
	if smtp.lastOpts.AuthMethod != "auto" || smtp.lastOpts.Username != "relayuser" || smtp.lastOpts.Password == nil {
		t.Fatalf("relay auth options wrong: %+v", smtp.lastOpts)
	}
}

func TestNotifyMailPasswordRotationRevokesOldReference(t *testing.T) {
	app, _, adminCtx, _ := notifyMailHarness(t)
	ctx := context.Background()
	refOf := func() domain.SecretRef {
		t.Helper()
		stored, _ := app.Store.GetSettings(ctx)
		ref := domain.SecretRef(stored[notifymail.KeyPassword])
		if !strings.HasPrefix(string(ref), "sec_") {
			t.Fatalf("expected a secret reference, got %q", ref)
		}
		return ref
	}
	usable := func(ref domain.SecretRef) bool {
		t.Helper()
		handle, err := app.Secrets.Acquire(ctx, ref, domain.PurposeTest)
		if err != nil {
			return false
		}
		handle.Zero()
		return true
	}
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Secrets: map[string]string{notifymail.KeyPassword: "first"}}); err != nil {
		t.Fatal(err)
	}
	first := refOf()
	// An empty secret preserves the stored password and its reference.
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.KeyUsername: "relayuser"}, Secrets: map[string]string{notifymail.KeyPassword: ""}}); err != nil {
		t.Fatal(err)
	}
	if refOf() != first || !usable(first) {
		t.Fatal("an empty secret must keep the existing password")
	}
	// Rotating through the REST patch revokes the superseded reference only.
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Secrets: map[string]string{notifymail.KeyPassword: "second"}}); err != nil {
		t.Fatal(err)
	}
	second := refOf()
	if second == first || usable(first) || !usable(second) {
		t.Fatalf("rotation must revoke %s and keep %s usable", first, second)
	}
	// The legacy endpoint rotates the same way.
	if err := app.AdminSaveSettings(adminCtx, map[string]string{notifymail.KeyPassword: "third"}, ""); err != nil {
		t.Fatal(err)
	}
	third := refOf()
	if third == second || usable(second) || !usable(third) {
		t.Fatalf("legacy rotation must revoke %s and keep %s usable", second, third)
	}
	// Clearing the password through the legacy endpoint revokes the last reference.
	if err := app.AdminSaveSettings(adminCtx, map[string]string{notifymail.KeyPassword: ""}, ""); err != nil {
		t.Fatal(err)
	}
	if stored, _ := app.Store.GetSettings(ctx); stored[notifymail.KeyPassword] != "" {
		t.Fatalf("password should be cleared, got %q", stored[notifymail.KeyPassword])
	}
	if usable(third) {
		t.Fatal("clearing the password must revoke its reference")
	}
}

func TestAdminSendTestMailReportsOutcome(t *testing.T) {
	app, smtp, adminCtx, hong := notifyMailHarness(t)
	if _, err := app.AdminSendTestMail(WithPrincipal(context.Background(), domain.Principal{UserID: hong.ID, Role: domain.RoleUser}), ""); err == nil {
		t.Fatal("non-admin must not send test mail")
	}
	if _, err := app.AdminSendTestMail(adminCtx, "not-an-address"); err == nil {
		t.Fatal("bad recipient must be rejected")
	}
	smtp.permFail = true
	d, err := app.AdminSendTestMail(adminCtx, "ops@corp.local")
	if err != nil || d.Status != domain.MailDeliveryFailed || d.Error == "" || d.Recipient != "ops@corp.local" {
		t.Fatalf("a dead relay is reported, not thrown: %+v %v", d, err)
	}
	list := waitNotifyMail(t, app)
	if len(list) != 1 || list[0].Status != domain.MailDeliveryFailed || list[0].Event != notifymail.EventTest {
		t.Fatalf("test sends are recorded too: %+v", list)
	}
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.KeyEnabled: "false"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.AdminSendTestMail(adminCtx, ""); err == nil {
		t.Fatal("disabled relay must refuse the test send with a reason")
	}
}

func TestAssignMessageMailsTheAssigneeButNotYourself(t *testing.T) {
	app, smtp, adminCtx, hong := notifyMailHarness(t)
	pop := app.POP3.(*fakePOP3)
	acc := mustAccount(t, app)
	pop.messages["u1"] = testMail("u1", "견적 요청", "body")
	syncAndWait(t, app, acc.ID)
	res, err := app.Store.Search(context.Background(), domain.SearchQuery{UserID: DefaultUserID, Limit: 1})
	if err != nil || len(res.Messages) != 1 {
		t.Fatalf("search: %v %+v", err, res)
	}
	msgID := res.Messages[0].ID
	if _, err := app.AssignMessage(adminCtx, msgID, "admin"); err != nil {
		t.Fatal(err)
	}
	if list := waitNotifyMail(t, app); len(list) != 0 {
		t.Fatalf("assigning to yourself must not mail you: %+v", list)
	}
	if _, err := app.AssignMessage(adminCtx, msgID, hong.LoginID); err != nil {
		t.Fatal(err)
	}
	list := waitNotifyMail(t, app)
	if len(list) != 1 || list[0].Event != notifymail.EventAssigned || list[0].Recipient != hong.Email || !strings.Contains(list[0].Subject, "견적 요청") || list[0].ActorID != DefaultUserID {
		t.Fatalf("assignee mail wrong: %+v", list)
	}
	if !strings.Contains(string(smtp.sent[0].raw), "/app/mail/"+msgID) {
		t.Fatal("assignment mail should deep-link to the message")
	}
	if _, err := app.AssignMessage(adminCtx, msgID, ""); err != nil {
		t.Fatal(err)
	}
	if list := waitNotifyMail(t, app); len(list) != 1 {
		t.Fatal("clearing an assignment is not mail-worthy")
	}
}

func TestSendFailureMailsTheSender(t *testing.T) {
	app, smtp, adminCtx, _ := notifyMailHarness(t)
	acc := mustAccount(t, app)
	ctx := WithPrincipal(WithActor(context.Background(), "test"), domain.Principal{UserID: DefaultUserID, LoginID: "admin", Role: domain.RoleAdmin})
	dv, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, Kind: "new", To: []string{"to@example.com"}, Subject: "월간 보고", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := app.RequestSendApproval(ctx, dv.Draft.ID, "me", 300)
	if err != nil {
		t.Fatal(err)
	}
	smtp.permFail = true
	out, err := app.Send(ctx, SendInput{DraftID: dv.Draft.ID, ApprovalToken: tok.Token})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != domain.OutboundFailed {
		t.Fatalf("expected failed outbound, got %s", out.Status)
	}
	list := waitNotifyMail(t, app)
	if len(list) != 1 || list[0].Event != notifymail.EventSendFailed || list[0].Recipient != "admin@corp.local" || !strings.Contains(list[0].Subject, "월간 보고") {
		t.Fatalf("sender was not told: %+v", list)
	}
	_ = adminCtx
}

func TestCriticalIncidentMailsAdminsOnce(t *testing.T) {
	app, _, _, hong := notifyMailHarness(t)
	app.recordIncident(domain.SeverityCritical, "sync", "panic: boom", "stack")
	app.recordIncident(domain.SeverityCritical, "sync", "panic: boom", "stack") // folded into the same row
	app.recordIncident(domain.SeverityWarning, "sync", "slow", "")              // not mail-worthy
	list := waitNotifyMail(t, app)
	if len(list) != 1 || list[0].Event != notifymail.EventIncident || list[0].Recipient != "admin@corp.local" {
		t.Fatalf("expected exactly one incident mail to the admin, got %+v", list)
	}
	if list[0].UserID == hong.ID {
		t.Fatal("plain users must not receive incident mail")
	}
}

func TestSLADeadlinesMailTheAssigneeOnceWhenImminentAndOnceWhenMissed(t *testing.T) {
	app, smtp, adminCtx, hong := notifyMailHarness(t)
	pop := app.POP3.(*fakePOP3)
	acc := mustAccount(t, app)
	pop.messages["u1"] = testMail("u1", "견적 요청", "body")
	pop.messages["u2"] = testMail("u2", "계약서 검토", "body")
	syncAndWait(t, app, acc.ID)
	res, err := app.Store.Search(context.Background(), domain.SearchQuery{UserID: DefaultUserID, Limit: 5})
	if err != nil || len(res.Messages) != 2 {
		t.Fatalf("search: %v %+v", err, res)
	}
	ids := map[string]string{}
	for _, m := range res.Messages {
		ids[m.Subject] = m.ID
	}
	quote, contract := ids["견적 요청"], ids["계약서 검토"]
	for _, id := range []string{quote, contract} {
		if _, err := app.AssignMessage(adminCtx, id, hong.LoginID); err != nil {
			t.Fatal(err)
		}
	}
	waitNotifyMail(t, app) // drain the two assignment mails
	smtp.sent = nil
	slaMails := func() []domain.MailDelivery {
		var out []domain.MailDelivery
		for _, d := range waitNotifyMail(t, app) {
			if d.Event == notifymail.EventSLADue {
				out = append(out, d)
			}
		}
		return out
	}
	now := time.Now().Truncate(time.Second)

	// Nothing is due within a day: silence.
	if _, err := app.SetMessageSLA(adminCtx, quote, now.Add(3*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if n := app.NotifySLADeadlines(adminCtx, now); n != 0 || len(slaMails()) != 0 {
		t.Fatalf("a deadline three days out is not imminent: %d %+v", n, slaMails())
	}

	// Imminent: one mail, deep-linked, and not repeated on the next pass.
	if _, err := app.SetMessageSLA(adminCtx, quote, now.Add(2*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if n := app.NotifySLADeadlines(adminCtx, now); n != 1 {
		t.Fatalf("expected one assignee mailed, got %d", n)
	}
	list := slaMails()
	if len(list) != 1 || list[0].Recipient != hong.Email || list[0].ActorID != "" || !strings.Contains(list[0].Subject, "임박") || !strings.Contains(list[0].Subject, "견적 요청") {
		t.Fatalf("imminent mail wrong: %+v", list)
	}
	if raw := string(smtp.sent[0].raw); !strings.Contains(raw, "/app/team?message="+quote) || !strings.Contains(raw, "24시간 안에") {
		t.Fatalf("imminent mail body wrong:\n%s", raw)
	}
	if n := app.NotifySLADeadlines(adminCtx, now.Add(time.Hour)); n != 0 || len(slaMails()) != 1 {
		t.Fatal("an imminent deadline is announced once")
	}

	// Missed: both deadlines have now passed, so hong gets one bundled mail.
	if _, err := app.SetMessageSLA(adminCtx, contract, now.Add(2*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	later := now.Add(3 * time.Hour)
	if n := app.NotifySLADeadlines(adminCtx, later); n != 1 {
		t.Fatalf("expected one bundled mail, got %d", n)
	}
	list = slaMails()
	if len(list) != 2 || list[0].Subject == list[1].Subject {
		t.Fatalf("expected a second, different mail: %+v", list)
	}
	overdue := findDelivery(list, "지났습니다")
	if overdue == nil || !strings.Contains(overdue.Subject, "2건") {
		t.Fatalf("missed-deadline mail wrong: %+v", list)
	}
	raw := string(smtp.sent[len(smtp.sent)-1].raw)
	for _, want := range []string{"기한이 지난 담당 메일 (2건)", "견적 요청", "계약서 검토", "/app/team\r\n"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("bundled mail lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "/app/team?message=") {
		t.Fatal("a bundle links to the team inbox, not one message")
	}
	if n := app.NotifySLADeadlines(adminCtx, later.Add(time.Hour)); n != 0 || len(slaMails()) != 2 {
		t.Fatal("a missed deadline is announced once")
	}

	// Done messages are left alone even when the deadline changes; the
	// system is the actor, so assigning yourself still gets the mail.
	if _, err := app.SetMessageWorkStatus(adminCtx, quote, domain.CollabDone); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetMessageSLA(adminCtx, quote, later.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.AssignMessage(adminCtx, contract, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetMessageSLA(adminCtx, contract, later.Unix()); err != nil {
		t.Fatal(err)
	}
	if n := app.NotifySLADeadlines(adminCtx, later.Add(time.Hour)); n != 1 {
		t.Fatalf("expected the admin's own deadline mail, got %d", n)
	}
	list = slaMails()
	if own := findDelivery(list, "지났습니다: 계약서 검토"); len(list) != 3 || own == nil || own.Recipient != "admin@corp.local" {
		t.Fatalf("self-set deadline should still be mailed by the system: %+v", list)
	}

	// The per-event switch stops only this kind.
	if _, err := app.AdminPatchSettings(adminCtx, SettingsPatch{Values: map[string]string{notifymail.NotifyKey(notifymail.EventSLADue): "false"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetMessageSLA(adminCtx, contract, later.Add(2*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if n := app.NotifySLADeadlines(adminCtx, later.Add(3*time.Hour)); n != 0 || len(slaMails()) != 3 {
		t.Fatal("switching sla_due off must silence it")
	}
	if _, err := app.AssignMessage(adminCtx, quote, hong.LoginID); err != nil {
		t.Fatal(err)
	}
	if all := waitNotifyMail(t, app); findDelivery(all, "배정") == nil {
		t.Fatal("other events keep flowing")
	}
}
