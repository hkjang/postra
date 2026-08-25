package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
)

func recentMessage(t *testing.T, app *App, ctx context.Context, accID, id, subject, body string) *domain.Message {
	t.Helper()
	now := time.Now().Unix()
	m := &domain.Message{
		ID: id, UserID: DefaultUserID, AccountID: accID, UIDL: "u-" + id,
		Subject: subject, From: domain.Address{Email: "sender@example.com"},
		RawHash: "h-" + id, RawURI: "mem://" + id, Date: now, CreatedAt: now,
	}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: id, TextBody: body}, nil); err != nil {
		t.Fatal(err)
	}
	return m
}

// A plain-language request becomes a validated rule — and is NOT saved, so the
// user confirms before anything starts acting on their mail.
func TestDraftRuleFromText(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ai.response = `{"name":"뉴스레터 보관","match":"any","stop_on_match":false,
	  "conditions":[{"field":"subject","operator":"contains","value":"newsletter"}],
	  "actions":[{"type":"add_label","value":"Archive"}]}`
	ctx := WithActor(context.Background(), "test")

	rule, err := app.DraftRuleFromText(ctx, "뉴스레터는 Archive 라벨로 보내줘")
	if err != nil {
		t.Fatal(err)
	}
	if len(rule.Conditions) != 1 || rule.Conditions[0].Field != "subject" {
		t.Fatalf("unexpected conditions: %+v", rule.Conditions)
	}
	if len(rule.Actions) != 1 || rule.Actions[0].Type != "add_label" {
		t.Fatalf("unexpected actions: %+v", rule.Actions)
	}
	if !rule.Enabled {
		t.Fatal("drafted rule should be enabled once saved")
	}
	// Preview only: nothing persisted yet.
	saved, err := app.ListRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 0 {
		t.Fatalf("draft must not be saved, found %d rules", len(saved))
	}

	// A model answer that is not a usable rule must be rejected, not stored.
	ai.response = `{"name":"x","conditions":[],"actions":[]}`
	if _, err := app.DraftRuleFromText(ctx, "그냥 잘 해줘"); err == nil {
		t.Fatal("expected an error for an unusable rule")
	}
}

// Calendar extraction drops ungroundable events and renders valid iCalendar.
func TestExtractCalendarEventsAndICS(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ai.response = `{"events":[
	  {"title":"제품 회의","start":"2026-03-04T15:00:00+09:00","end":"2026-03-04T16:00:00+09:00","all_day":false,"location":"회의실 A; 3층","confidence":0.9},
	  {"title":"마감일","start":"2026-03-10","all_day":true,"confidence":0.8},
	  {"title":"쓸모없는 항목","start":"","all_day":false}]}`
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	recentMessage(t, app, ctx, acc.ID, "msg_cal", "회의 안내", "3월 4일 15시 회의입니다.")

	out, err := app.ExtractCalendarEvents(ctx, "msg_cal")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 2 {
		t.Fatalf("want 2 usable events (unparseable one dropped), got %d", len(out.Events))
	}
	ics := out.ICS()
	for _, want := range []string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "DTSTART:20260304T060000Z",
		"DTSTART;VALUE=DATE:20260310", "END:VCALENDAR"} {
		if !strings.Contains(ics, want) {
			t.Errorf("ICS missing %q\n%s", want, ics)
		}
	}
	// RFC 5545 requires semicolons in values to be escaped.
	if !strings.Contains(ics, `회의실 A\; 3층`) {
		t.Errorf("semicolon not escaped in LOCATION:\n%s", ics)
	}
	// An all-day event with no end still needs a valid DTEND.
	if !strings.Contains(ics, "DTEND;VALUE=DATE:20260311") {
		t.Errorf("all-day event missing derived DTEND:\n%s", ics)
	}
}

// The digest summarizes recent mail and refuses to invent one from nothing.
func TestGenerateDailyDigest(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ai.response = `{"headline":"오늘은 승인 요청 1건이 있습니다.","needs_reply":[],"deadlines":[],"fyi":[],"volume_note":"1건"}`
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)

	if _, err := app.GenerateDailyDigest(ctx, "", 24); err == nil {
		t.Fatal("expected an error when there is no recent mail")
	}
	recentMessage(t, app, ctx, acc.ID, "msg_dig", "승인 요청", "예산 승인 부탁드립니다.")

	an, err := app.GenerateDailyDigest(ctx, "", 24)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(an.ResultJSON, "headline") {
		t.Fatalf("digest missing headline: %s", an.ResultJSON)
	}
	// Mail content must stay in the untrusted channel.
	if !strings.Contains(ai.lastRequest.Untrusted, "예산 승인") {
		t.Fatal("digest did not pass mail through the untrusted block")
	}
}

// Auto-triage labels new mail by urgency so the inbox can be read by priority,
// and flags urgent/high mail important.
func TestTriageWorkerLabelsNewMail(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ai.response = `{"sender_intent":"승인 요청","priority":"urgent","reply_required":true,
	  "deadline":null,"business_risks":[],"recommended_next_action":"확인","confidence":0.9}`
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	recentMessage(t, app, ctx, acc.ID, "msg_tri", "긴급 승인", "오늘 중 승인 필요합니다.")

	if n := app.triageOnce(ctx); n != 1 {
		t.Fatalf("triaged %d messages, want 1", n)
	}
	m, err := app.Store.GetMessage(ctx, DefaultUserID, "msg_tri")
	if err != nil {
		t.Fatal(err)
	}
	var hasPriority, hasReply bool
	for _, l := range m.Labels {
		switch l {
		case "ai/urgent":
			hasPriority = true
		case "ai/reply-needed":
			hasReply = true
		}
	}
	if !hasPriority {
		t.Fatalf("no ai/urgent label applied: %v", m.Labels)
	}
	if !hasReply {
		t.Fatalf("no ai/reply-needed label applied: %v", m.Labels)
	}
	if !m.IsImportant {
		t.Fatal("urgent mail should be flagged important")
	}
	// Already-triaged mail must not be re-processed (no repeated AI spend).
	if n := app.triageOnce(ctx); n != 0 {
		t.Fatalf("re-triaged %d already-labelled messages, want 0", n)
	}
}
