package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// Mail whose substance lives in an attached document must be indexed by that
// content, not just by its cover note.
func TestAttachmentTextJoinsIndexAndRAG(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)

	const attachedFact = "2026년 예산은 1억 2천만원으로 확정되었습니다"
	uri, hash, _, err := app.Objects.Put("att", strings.NewReader(attachedFact))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_att", UserID: DefaultUserID, AccountID: acc.ID, UIDL: "u-att",
		Subject: "자료 전달", From: domain.Address{Email: "sender@example.com"},
		RawHash: "h-att", RawURI: "mem://att", Date: now, CreatedAt: now,
		HasAttachments: true,
	}
	atts := []domain.Attachment{{
		ID: "att_1", MessageID: m.ID, Name: "budget.txt", MIMEType: "text/plain",
		Size: int64(len(attachedFact)), Hash: hash, StorageURI: uri,
		ScanStatus: domain.ScanClean,
	}}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID, TextBody: "첨부 확인 바랍니다."}, atts); err != nil {
		t.Fatal(err)
	}

	got := app.AttachmentsTextForIndex(ctx, m.ID, 1500)
	if !strings.Contains(got, attachedFact) {
		t.Fatalf("attachment text not available for indexing: %q", got)
	}
	if !strings.Contains(got, "budget.txt") {
		t.Fatalf("attachment name should label the excerpt: %q", got)
	}
	// The budget must respect the caller's cap.
	if capped := app.AttachmentsTextForIndex(ctx, m.ID, 20); len([]rune(capped)) > 80 {
		t.Fatalf("budget ignored, got %d runes", len([]rune(capped)))
	}
}

// Blocked attachments were deliberately not retained; indexing must not try to
// read them back.
func TestAttachmentTextSkipsBlocked(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_blk", UserID: DefaultUserID, AccountID: acc.ID, UIDL: "u-blk",
		Subject: "차단 첨부", From: domain.Address{Email: "x@example.com"},
		RawHash: "h-blk", RawURI: "mem://blk", Date: now, CreatedAt: now, HasAttachments: true,
	}
	atts := []domain.Attachment{{
		ID: "att_blk", MessageID: m.ID, Name: "evil.exe", MIMEType: "text/plain",
		StorageURI: "", ScanStatus: domain.ScanBlocked,
	}}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID}, atts); err != nil {
		t.Fatal(err)
	}
	if got := app.AttachmentsTextForIndex(ctx, m.ID, 1500); got != "" {
		t.Fatalf("blocked attachment must not be indexed, got %q", got)
	}
}

// Indexing must read only the prefix it keeps. A large attachment previously
// pulled up to 5 MB into memory per message to retain ~1500 characters, which
// an embedding batch multiplies into hundreds of megabytes.
func TestAttachmentIndexReadIsBounded(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)

	big := strings.Repeat("가나다라마바사아자차", 200000) // ~2M runes
	uri, hash, _, err := app.Objects.Put("att", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_big", UserID: DefaultUserID, AccountID: acc.ID, UIDL: "u-big",
		Subject: "대용량", From: domain.Address{Email: "x@example.com"},
		RawHash: "h-big", RawURI: "mem://big", Date: now, CreatedAt: now, HasAttachments: true,
	}
	atts := []domain.Attachment{{
		ID: "att_big", MessageID: m.ID, Name: "huge.txt", MIMEType: "text/plain",
		Hash: hash, StorageURI: uri, ScanStatus: domain.ScanClean,
	}}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID}, atts); err != nil {
		t.Fatal(err)
	}

	got := app.AttachmentsTextForIndex(ctx, m.ID, 1500)
	if got == "" {
		t.Fatal("large attachment produced no indexable text")
	}
	// Header line plus at most the budget; nowhere near the 2M-rune source.
	if n := len([]rune(got)); n > 1600 {
		t.Fatalf("index text not bounded: %d runes", n)
	}
	if !strings.Contains(got, "huge.txt") {
		t.Fatalf("attachment name missing: %q", got[:80])
	}
	// Truncating a multi-byte stream must not leave invalid UTF-8 behind.
	if !utf8.ValidString(got) {
		t.Fatal("index text contains invalid UTF-8 from a mid-rune cut")
	}
}

// Quarantined content sits behind an acknowledgement gate for manual
// downloads; background indexing must not read it either.
func TestAttachmentIndexSkipsQuarantined(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)

	uri, hash, _, err := app.Objects.Put("att", strings.NewReader("격리된 내용"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_q", UserID: DefaultUserID, AccountID: acc.ID, UIDL: "u-q",
		Subject: "격리", From: domain.Address{Email: "x@example.com"},
		RawHash: "h-q", RawURI: "mem://q", Date: now, CreatedAt: now, HasAttachments: true,
	}
	atts := []domain.Attachment{{
		ID: "att_q", MessageID: m.ID, Name: "macro.txt", MIMEType: "text/plain",
		Hash: hash, StorageURI: uri, ScanStatus: domain.ScanQuarantined,
	}}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID}, atts); err != nil {
		t.Fatal(err)
	}
	if got := app.AttachmentsTextForIndex(ctx, m.ID, 1500); got != "" {
		t.Fatalf("quarantined attachment must not be indexed, got %q", got)
	}
}

// Indexing is a background read, not a user download: it must not forge an
// attachment_download audit record against the user.
func TestAttachmentIndexWritesNoDownloadAudit(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)

	uri, hash, _, err := app.Objects.Put("att", strings.NewReader("색인 대상 텍스트"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_aud", UserID: DefaultUserID, AccountID: acc.ID, UIDL: "u-aud",
		Subject: "감사", From: domain.Address{Email: "x@example.com"},
		RawHash: "h-aud", RawURI: "mem://aud", Date: now, CreatedAt: now, HasAttachments: true,
	}
	atts := []domain.Attachment{{
		ID: "att_aud", MessageID: m.ID, Name: "notes.txt", MIMEType: "text/plain",
		Hash: hash, StorageURI: uri, ScanStatus: domain.ScanClean,
	}}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID}, atts); err != nil {
		t.Fatal(err)
	}
	if got := app.AttachmentsTextForIndex(ctx, m.ID, 1500); got == "" {
		t.Fatal("clean attachment should be indexed")
	}
	events, err := app.Store.SearchAudit(ctx, DefaultUserID, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == "attachment_download" {
			t.Fatalf("indexing forged a download audit record: %+v", e)
		}
	}
}

// A message whose analysis cannot be parsed must still leave the triage queue.
// Analyses are cached by input hash, so retrying returns the same unusable
// output forever — the message would occupy a batch slot and burn an AI call
// on every tick.
func TestTriageDoesNotRetryUnusableOutputForever(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ai.response = `{"priority": ` // malformed on purpose
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	recentMessage(t, app, ctx, acc.ID, "msg_bad", "형식 오류", "본문")

	if n := app.triageOnce(ctx); n != 1 {
		t.Fatalf("message should still be labelled with the neutral default, got n=%d", n)
	}
	m, err := app.Store.GetMessage(ctx, DefaultUserID, "msg_bad")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(m.Labels); got == 0 {
		t.Fatal("no label applied, message will be retried forever")
	}
	// Second pass must find nothing left to do.
	if n := app.triageOnce(ctx); n != 0 {
		t.Fatalf("message re-triaged after being labelled (n=%d)", n)
	}
}

// When the provider is down, the pass stops instead of repeating the same
// failure against every remaining message.
func TestTriageStopsWhenProviderUnavailable(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ai.genErr = errors.New("provider unreachable")
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	recentMessage(t, app, ctx, acc.ID, "msg_d1", "하나", "본문")
	recentMessage(t, app, ctx, acc.ID, "msg_d2", "둘", "본문")

	if n := app.triageOnce(ctx); n != 0 {
		t.Fatalf("nothing should be labelled during an outage, got %d", n)
	}
	for _, id := range []string{"msg_d1", "msg_d2"} {
		m, err := app.Store.GetMessage(ctx, DefaultUserID, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Labels) != 0 {
			t.Fatalf("%s labelled despite the outage: %v", id, m.Labels)
		}
	}
	// Once the provider recovers, the same mail is picked up.
	ai.genErr = nil
	ai.response = `{"priority":"high","reply_required":false}`
	if n := app.triageOnce(ctx); n != 2 {
		t.Fatalf("expected both messages triaged after recovery, got %d", n)
	}
}

// A transient provider fault must not cost the user their whole day's
// briefing: the day stays unmarked so the next tick retries.
func TestDigestRetriesAfterTransientFailure(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	app.Cfg.Sync.DailyDigestHour = 0 // the hour gate is not what we are testing
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	recentMessage(t, app, ctx, acc.ID, "msg_dg", "보고", "확인 부탁드립니다.")

	ai.genErr = errors.New("provider unreachable")
	app.digestOnce(ctx)
	settings, err := app.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, marked := settings[digestStateKey+DefaultUserID]; marked {
		t.Fatal("day marked done despite a transient failure — briefing would be lost")
	}

	ai.genErr = nil
	ai.response = `{"headline":"오늘의 요약","needs_reply":[],"deadlines":[],"fyi":[],"volume_note":"1건"}`
	app.digestOnce(ctx)
	settings, err = app.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, marked := settings[digestStateKey+DefaultUserID]; !marked {
		t.Fatal("successful briefing did not mark the day, it would regenerate every tick")
	}
}
