package webui

import (
	"context"
	"net/http"
	"net/url"
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
