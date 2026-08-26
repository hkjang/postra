package webui

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"postra/internal/application"
	"postra/internal/domain"
)

func TestSmartTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"today shows the clock", now.Add(-2 * time.Hour), now.Add(-2 * time.Hour).Format("15:04")},
		{"yesterday is named", now.AddDate(0, 0, -1), "어제"},
		{"this year drops the year", time.Date(now.Year(), 1, 2, 9, 0, 0, 0, now.Location()), "1월 2일"},
		{"older keeps the year", time.Date(now.Year()-2, 3, 4, 9, 0, 0, 0, now.Location()), time.Date(now.Year()-2, 3, 4, 9, 0, 0, 0, now.Location()).Format("2006-01-02")},
	}
	for _, c := range cases {
		// The "this year" case collapses into today/yesterday/weekday when the
		// run happens to fall near Jan 2; skip rather than assert a false rule.
		if c.name == "this year drops the year" && now.Sub(c.at) < 7*24*time.Hour {
			continue
		}
		if got := smartTime(c.at.Unix()); got != c.want {
			t.Errorf("%s: smartTime = %q, want %q", c.name, got, c.want)
		}
	}

	if got := smartTime(0); got != "—" {
		t.Errorf("missing date should render as a dash, got %q", got)
	}
	// A sender's clock can be wrong; a future stamp must not read as "in 3h".
	future := now.Add(48 * time.Hour)
	if got := smartTime(future.Unix()); got != future.Format("2006-01-02") {
		t.Errorf("future timestamp = %q, want the plain date", got)
	}
}

func TestSenderName(t *testing.T) {
	if got := senderName(domain.Address{Name: "김철수", Email: "kim@corp.local"}); got != "김철수" {
		t.Errorf("display name should win, got %q", got)
	}
	if got := senderName(domain.Address{Email: "noname@corp.local"}); got != "noname@corp.local" {
		t.Errorf("address is the fallback, got %q", got)
	}
	if got := senderName(domain.Address{Name: "   ", Email: "blank@corp.local"}); got != "blank@corp.local" {
		t.Errorf("whitespace-only name must not render as a blank sender, got %q", got)
	}
}

// Render the inbox for real: a template that parses can still fail at render,
// and the row markup is what the user actually sees.
func TestInboxRendersMailRows(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_ui", UserID: application.DefaultUserID, AccountID: "acc_ui", UIDL: "u-ui",
		Subject: "분기 예산 검토 요청", From: domain.Address{Name: "김철수", Email: "kim@corp.local"},
		RawHash: "h-ui", RawURI: "mem://ui", Date: now, CreatedAt: now,
		IsImportant: true, HasAttachments: true, Size: 2048,
		Labels: []string{"ai/urgent", "ai/reply-needed"},
	}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID, TextBody: "본문"}, nil); err != nil {
		t.Fatal(err)
	}
	// InsertMessage stores the ingested fields only; labels and the important
	// flag are applied afterwards (by triage or the user), as they are here.
	if err := app.Store.UpdateMessage(ctx, m); err != nil {
		t.Fatal(err)
	}

	rec := do(t, New(app, "").Handler(), http.MethodGet, "/ui/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox returned %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="mailrow`,           // the redesigned row rendered
		"is-important",             // importance is visible
		"분기 예산 검토 요청",              // subject
		"김철수",                      // display name, not "Name <addr>"
		`class="prio prio-urgent"`, // AI priority badge
		"답장 필요",
		time.Unix(now, 0).Format("15:04"), // today's mail shows the clock
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox markup missing %q", want)
		}
	}
	// The raw "Name <email>" form should no longer appear in the list.
	if strings.Contains(body, "김철수 &lt;kim@corp.local&gt;") {
		t.Error("list still renders the verbose Name <email> form")
	}
}

func TestSnippetOf(t *testing.T) {
	// Mail is full of hard wraps and blank lines; a preview must read as one line.
	got := snippetOf("안녕하세요.\n\n   내일 회의\t자료를\r\n보냅니다.  ", 140)
	if got != "안녕하세요. 내일 회의 자료를 보냅니다." {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
	if snippetOf("   \n\t  ", 140) != "" {
		t.Error("a blank body should yield no preview at all")
	}
	// Truncation counts runes, not bytes, so Korean text is not cut mid-character.
	long := strings.Repeat("가", 300)
	cut := snippetOf(long, 10)
	if r := []rune(cut); len(r) != 11 || string(r[:10]) != strings.Repeat("가", 10) || r[10] != '…' {
		t.Fatalf("truncation is not rune-aware: %q", cut)
	}
	if !utf8.ValidString(cut) {
		t.Fatal("truncation produced invalid UTF-8")
	}
}

// The preview must reach the rendered row, and come from one batched query
// rather than a per-row fetch.
func TestInboxRendersPreview(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_prev", UserID: application.DefaultUserID, AccountID: "acc_p", UIDL: "u-p",
		Subject: "회의 안내", From: domain.Address{Name: "박영희", Email: "park@corp.local"},
		RawHash: "h-p", RawURI: "mem://p", Date: now, CreatedAt: now,
	}
	body := &domain.MessageBody{MessageID: m.ID, TextBody: "안녕하세요.\n\n내일 3시 회의 가능하신가요?"}
	if err := app.Store.InsertMessage(ctx, m, body, nil); err != nil {
		t.Fatal(err)
	}

	rec := do(t, New(app, "").Handler(), http.MethodGet, "/ui/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox returned %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `class="mailrow-snippet">안녕하세요. 내일 3시 회의 가능하신가요?<`) {
		t.Fatalf("preview missing from the row:\n%s", rec.Body.String())
	}

	// Bodies are encrypted at rest in this configuration, so the batch fetch
	// must decrypt — otherwise the preview would show ciphertext.
	texts, err := app.Store.BodyTextBatch(ctx, application.DefaultUserID, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(texts[m.ID], "내일 3시 회의") {
		t.Fatalf("batch fetch did not return decrypted text: %q", texts[m.ID])
	}
	// Unknown IDs are simply absent, not an error that would break the listing.
	if got, err := app.Store.BodyTextBatch(ctx, application.DefaultUserID, []string{"nope"}); err != nil || len(got) != 0 {
		t.Fatalf("unknown id should yield an empty map, got %v (err=%v)", got, err)
	}
}

// Bulk actions must act on exactly the ticked messages and report the outcome,
// including partial failure — a silent "done" would hide it.
func TestInboxBulkActions(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")
	now := time.Now().Unix()
	for _, id := range []string{"msg_b1", "msg_b2"} {
		m := &domain.Message{
			ID: id, UserID: application.DefaultUserID, AccountID: "acc_b", UIDL: "u-" + id,
			Subject: id, From: domain.Address{Email: "x@corp.local"},
			RawHash: "h-" + id, RawURI: "mem://" + id, Date: now, CreatedAt: now,
		}
		if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: id}, nil); err != nil {
			t.Fatal(err)
		}
	}
	h := New(app, "").Handler()

	// The list offers the controls.
	body := do(t, h, http.MethodGet, "/ui/", nil, nil).Body.String()
	for _, want := range []string{`name="ids"`, `value="archive"`, `value="mark_important"`, `value="delete"`} {
		if !strings.Contains(body, want) {
			t.Errorf("bulk control %q missing from the list", want)
		}
	}

	// Marking one important must leave the other untouched.
	rec := do(t, h, http.MethodPost, "/ui/messages/batch",
		url.Values{"ids": {"msg_b1"}, "action": {"mark_important"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("batch returned %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "done=1") || !strings.Contains(loc, "failed=0") {
		t.Fatalf("outcome not reported back to the list: %q", loc)
	}
	got, err := app.Store.GetMessage(ctx, application.DefaultUserID, "msg_b1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsImportant {
		t.Fatal("ticked message was not marked important")
	}
	other, err := app.Store.GetMessage(ctx, application.DefaultUserID, "msg_b2")
	if err != nil {
		t.Fatal(err)
	}
	if other.IsImportant {
		t.Fatal("an unticked message was modified")
	}

	// Submitting with nothing ticked must not be reported as work done.
	rec = do(t, h, http.MethodPost, "/ui/messages/batch", url.Values{"action": {"archive"}}, nil)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "none=1") {
		t.Fatalf("empty selection should say so, got %q", loc)
	}

	// A partial failure is surfaced, not swallowed.
	rec = do(t, h, http.MethodPost, "/ui/messages/batch",
		url.Values{"ids": {"msg_b2", "msg_missing"}, "action": {"archive"}}, nil)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "done=1") || !strings.Contains(loc, "failed=1") {
		t.Fatalf("partial failure not reported: %q", loc)
	}
}

// Reading a message and filing it is the common pair, so the detail view must
// offer the actions itself — and reflect the state it just changed.
func TestMessageDetailActions(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_act", UserID: application.DefaultUserID, AccountID: "acc_a", UIDL: "u-act",
		Subject: "처리 대상", From: domain.Address{Email: "x@corp.local"},
		RawHash: "h-act", RawURI: "mem://act", Date: now, CreatedAt: now,
	}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID, TextBody: "본문"}, nil); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()

	// Not important yet: the view offers to mark it.
	body := do(t, h, http.MethodGet, "/ui/messages/msg_act", nil, nil).Body.String()
	if !strings.Contains(body, `value="mark_important"`) {
		t.Fatal("detail view offers no way to mark the message important")
	}

	rec := do(t, h, http.MethodPost, "/ui/messages/msg_act/action",
		url.Values{"action": {"mark_important"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("action returned %d", rec.Code)
	}
	// A reversible flag keeps the reader where they were.
	if loc := rec.Header().Get("Location"); loc != "/ui/messages/msg_act" {
		t.Fatalf("marking important should stay on the message, went to %q", loc)
	}
	got, err := app.Store.GetMessage(ctx, application.DefaultUserID, "msg_act")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsImportant {
		t.Fatal("message was not marked important")
	}

	// Now the control flips rather than offering the same thing twice.
	body = do(t, h, http.MethodGet, "/ui/messages/msg_act", nil, nil).Body.String()
	if !strings.Contains(body, `value="unmark_important"`) || strings.Contains(body, `value="mark_important"`) {
		t.Fatal("the important control did not flip to its inverse")
	}

	// Archiving removes it from the inbox view, so return to the list.
	rec = do(t, h, http.MethodPost, "/ui/messages/msg_act/action", url.Values{"action": {"archive"}}, nil)
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Fatalf("archiving should return to the list, went to %q", loc)
	}
	got, err = app.Store.GetMessage(ctx, application.DefaultUserID, "msg_act")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsArchived {
		t.Fatal("message was not archived")
	}
}

// A conversation was only reachable over CLI and MCP; the browser had no way
// to see the replies that make a message make sense.
func TestThreadView(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")
	now := time.Now().Unix()
	// Five messages so the collapse rule (newest few expanded) actually applies.
	for i := 0; i < 5; i++ {
		id := "msg_t" + strconv.Itoa(i)
		m := &domain.Message{
			ID: id, UserID: application.DefaultUserID, AccountID: "acc_t", UIDL: "u-" + id,
			Subject: "예산 협의", From: domain.Address{Name: "참여자" + strconv.Itoa(i), Email: "p@corp.local"},
			To:      []domain.Address{{Email: "me@corp.local"}},
			RawHash: "h-" + id, RawURI: "mem://" + id,
			Date: now - int64((5-i)*3600), CreatedAt: now, ThreadID: "thr_1",
		}
		if err := app.Store.InsertMessage(ctx, m,
			&domain.MessageBody{MessageID: id, TextBody: "내용 " + strconv.Itoa(i)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	h := New(app, "").Handler()

	rec := do(t, h, http.MethodGet, "/ui/threads/thr_1", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("thread page returned %d", rec.Code)
	}
	body := rec.Body.String()
	// Every message is accounted for: the newest expanded, the rest as briefs.
	if n := strings.Count(body, `class="tmsg-brief"`); n != 2 {
		t.Fatalf("collapsed %d of 5 messages, want the 2 oldest", n)
	}
	if n := strings.Count(body, `<article class="tmsg`); n != 3 {
		t.Fatalf("expanded %d messages, want the 3 newest", n)
	}
	// The newest bodies are shown...
	for _, want := range []string{"내용 2", "내용 3", "내용 4"} {
		if !strings.Contains(body, want) {
			t.Errorf("expanded message body %q missing", want)
		}
	}
	// ...and the collapsed ones are not fetched or shipped at all. Rendering
	// them hidden would still send the whole conversation to the browser.
	for _, unwanted := range []string{"내용 0", "내용 1"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("collapsed message body %q was sent to the browser", unwanted)
		}
	}

	// The detail view offers the conversation, with its size.
	msgPage := do(t, h, http.MethodGet, "/ui/messages/msg_t0", nil, nil).Body.String()
	if !strings.Contains(msgPage, `href="/ui/threads/thr_1"`) || !strings.Contains(msgPage, "대화 보기 (5)") {
		t.Fatal("message detail does not link to its conversation")
	}
}

// A message that is its own thread must not advertise a conversation.
func TestLoneMessageHasNoThreadLink(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_solo", UserID: application.DefaultUserID, AccountID: "acc_s", UIDL: "u-solo",
		Subject: "단독", From: domain.Address{Email: "x@corp.local"},
		RawHash: "h-solo", RawURI: "mem://solo", Date: now, CreatedAt: now, ThreadID: "thr_solo",
	}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID, TextBody: "본문"}, nil); err != nil {
		t.Fatal(err)
	}
	body := do(t, New(app, "").Handler(), http.MethodGet, "/ui/messages/msg_solo", nil, nil).Body.String()
	if strings.Contains(body, "대화 보기") {
		t.Fatal("a single-message thread should not offer a conversation link")
	}
}

// The header should say where the reader is — both visually and to a screen
// reader — and detail pages belong to the section they were reached from.
func TestNavigationMarksCurrentSection(t *testing.T) {
	if got := navSection("message"); got != "inbox" {
		t.Errorf("a message belongs to the inbox, got %q", got)
	}
	if got := navSection("thread"); got != "inbox" {
		t.Errorf("a conversation belongs to the inbox, got %q", got)
	}
	if got := navSection("draft"); got != "compose" {
		t.Errorf("a draft belongs to composing, got %q", got)
	}
	if got := navSection("account_new"); got != "accounts" {
		t.Errorf("adding an account belongs to accounts, got %q", got)
	}
	if got := navSection("login"); got != "" {
		t.Errorf("pages outside the app shell mark nothing, got %q", got)
	}

	app, _ := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_nav", UserID: application.DefaultUserID, AccountID: "acc_n", UIDL: "u-nav",
		Subject: "탐색", From: domain.Address{Email: "x@corp.local"},
		RawHash: "h-nav", RawURI: "mem://nav", Date: now, CreatedAt: now,
	}
	if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID, TextBody: "본문"}, nil); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()

	// Reading a message still shows the inbox as the current section.
	body := do(t, h, http.MethodGet, "/ui/messages/msg_nav", nil, nil).Body.String()
	if !strings.Contains(body, `<a href="/ui/" class="on" aria-current="page">받은편지함</a>`) {
		t.Fatal("the inbox is not marked current while reading a message")
	}
	// Exactly one entry is current, or the header points two ways at once.
	if n := strings.Count(body, `aria-current="page"`); n != 1 {
		t.Fatalf("%d nav entries marked current, want 1", n)
	}
	// Keyboard users need a way past the header.
	if !strings.Contains(body, `class="skip-link" href="#main"`) || !strings.Contains(body, `<main id="main"`) {
		t.Fatal("no skip-to-content link targeting the main landmark")
	}

	// A different section marks itself, not the inbox.
	rules := do(t, h, http.MethodGet, "/ui/rules", nil, nil).Body.String()
	if !strings.Contains(rules, `<a href="/ui/rules" class="on" aria-current="page">규칙</a>`) {
		t.Fatal("the rules page does not mark itself current")
	}
}

// DLP is enforced at send time so an approval cannot be edited around. But the
// preview already knows the send is blocked, so it must say so up front rather
// than walking the user through approval to fail on the final click.
func TestSendPreviewSurfacesDLPBlock(t *testing.T) {
	app, _ := newTestApp(t)
	app.Cfg.Send.DLPPolicy = "block"
	app.Cfg.Send.DLPKeywords = []string{"대외비"}
	ctx := application.WithActor(context.Background(), "test")

	acc, err := app.CreateAccount(ctx, application.CreateAccountInput{
		Name: "회사", Email: "me@corp.local",
		POP3Host: "127.0.0.1", POP3Port: 1100, POP3Security: "none", POP3Username: "me",
		SMTPHost: "127.0.0.1", SMTPPort: 1025, SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	dv, err := app.CreateDraft(ctx, application.CreateDraftInput{
		AccountID: acc.ID, Kind: "new",
		To:      []string{"outsider@other.example"}, // external domain triggers DLP
		Subject: "자료 전달", Body: "대외비 문서를 첨부합니다.",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()

	body := do(t, h, http.MethodGet, "/ui/drafts/"+dv.Draft.ID+"/send", nil, nil).Body.String()
	if !strings.Contains(body, "DLP 정책에 의해 발송이 차단됩니다") {
		t.Fatal("the preview does not say the send is blocked")
	}
	if !strings.Contains(body, "대외비") {
		t.Fatal("the preview does not show what triggered the block")
	}
	// The approval button must not be offered when it cannot succeed.
	if strings.Contains(body, `value="approve"`) {
		t.Fatal("an approval that cannot succeed is still offered")
	}
	if !strings.Contains(body, "초안 수정하기") {
		t.Fatal("no route back to fixing the draft")
	}

	// With the offending term removed, approval becomes available again.
	if _, err := app.UpdateDraft(ctx, application.UpdateDraftInput{
		DraftID: dv.Draft.ID, Body: strPtr("일반 문서를 첨부합니다."),
	}); err != nil {
		t.Fatal(err)
	}
	body = do(t, h, http.MethodGet, "/ui/drafts/"+dv.Draft.ID+"/send", nil, nil).Body.String()
	if !strings.Contains(body, `value="approve"`) {
		t.Fatal("a clean draft should be approvable")
	}
	if strings.Contains(body, "DLP 정책에 의해 발송이 차단됩니다") {
		t.Fatal("the block notice survived the fix")
	}
}

func strPtr(s string) *string { return &s }

// Archiving and snoozing only mean something if the inbox stops showing the
// message. The list never asked for a folder, so the store applied no filter
// and both actions changed a flag while the mail stayed exactly where it was.
func TestFoldersHideAndRecoverMail(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")
	now := time.Now().Unix()
	for _, spec := range []struct{ id, subj string }{
		{"msg_keep", "그대로 둘 메일"},
		{"msg_arch", "보관할 메일"},
		{"msg_snz", "미뤄둘 메일"},
	} {
		m := &domain.Message{
			ID: spec.id, UserID: application.DefaultUserID, AccountID: "acc_f", UIDL: "u-" + spec.id,
			Subject: spec.subj, From: domain.Address{Email: "x@corp.local"},
			RawHash: "h-" + spec.id, RawURI: "mem://" + spec.id, Date: now, CreatedAt: now,
		}
		if err := app.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: spec.id}, nil); err != nil {
			t.Fatal(err)
		}
	}
	h := New(app, "").Handler()

	do(t, h, http.MethodPost, "/ui/messages/msg_arch/action", url.Values{"action": {"archive"}}, nil)
	do(t, h, http.MethodPost, "/ui/messages/msg_snz/action",
		url.Values{"action": {"snooze"}, "snooze": {"week"}}, nil)

	inbox := do(t, h, http.MethodGet, "/ui/", nil, nil).Body.String()
	if !strings.Contains(inbox, "그대로 둘 메일") {
		t.Fatal("ordinary mail vanished from the inbox")
	}
	if strings.Contains(inbox, "보관할 메일") {
		t.Error("archived mail is still listed in the inbox")
	}
	if strings.Contains(inbox, "미뤄둘 메일") {
		t.Error("snoozed mail is still listed in the inbox")
	}

	// Filed mail must be recoverable, or archiving is indistinguishable from
	// deleting.
	archive := do(t, h, http.MethodGet, "/ui/?folder=archive", nil, nil).Body.String()
	if !strings.Contains(archive, "보관할 메일") || strings.Contains(archive, "그대로 둘 메일") {
		t.Error("the archive folder does not show exactly the archived mail")
	}
	snoozed := do(t, h, http.MethodGet, "/ui/?folder=snoozed", nil, nil).Body.String()
	if !strings.Contains(snoozed, "미뤄둘 메일") {
		t.Error("the snoozed folder does not show the snoozed mail")
	}
	// The archive view offers the way back rather than only the way in.
	if !strings.Contains(archive, `value="unarchive"`) {
		t.Error("no way to unarchive from the archive view")
	}

	// Unarchiving returns it to the inbox.
	do(t, h, http.MethodPost, "/ui/messages/batch",
		url.Values{"ids": {"msg_arch"}, "action": {"unarchive"}, "folder": {"archive"}}, nil)
	inbox = do(t, h, http.MethodGet, "/ui/", nil, nil).Body.String()
	if !strings.Contains(inbox, "보관할 메일") {
		t.Error("unarchived mail did not return to the inbox")
	}
}

// A snooze is only meaningful against a clock.
func TestSnoozeUntilChoices(t *testing.T) {
	now := time.Now()
	if got := snoozeUntil("week"); got < now.AddDate(0, 0, 6).Unix() || got > now.AddDate(0, 0, 8).Unix() {
		t.Errorf("week snooze landed outside the expected range: %d", got)
	}
	if got := snoozeUntil("3d"); got <= now.Unix() {
		t.Errorf("3d snooze is not in the future: %d", got)
	}
	// Tomorrow morning must be tomorrow, and in the morning.
	tm := time.Unix(snoozeUntil("tomorrow"), 0)
	if tm.Before(now) {
		t.Error("tomorrow-morning snooze is in the past")
	}
	if tm.Hour() != 8 {
		t.Errorf("tomorrow-morning snooze is at %02d:00, want 08:00", tm.Hour())
	}
	// An unknown choice must still produce a future time, not zero (which the
	// batch API rejects, and which would read as "never snoozed").
	if got := snoozeUntil("garbage"); got <= now.Unix() {
		t.Errorf("unknown choice produced %d, want a future time", got)
	}
}
