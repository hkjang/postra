package webui

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"postra/internal/application"
	"postra/internal/domain"
)

type richMailSMTP struct {
	raw   []byte
	sends int
	opts  domain.SMTPSendOptions
}

func (*richMailSMTP) TestConnection(context.Context, domain.SMTPSendOptions) (*domain.ConnDiagnostics, error) {
	return &domain.ConnDiagnostics{Target: "smtp", OK: true}, nil
}

func (s *richMailSMTP) Send(_ context.Context, opts domain.SMTPSendOptions, _ domain.Envelope, raw io.Reader) (domain.SendReceipt, error) {
	var err error
	s.raw, err = io.ReadAll(raw)
	s.sends++
	s.opts = opts
	return domain.SendReceipt{ServerResponse: "250 OK"}, err
}

func richMailAccount(t *testing.T, app *application.App) string {
	t.Helper()
	ctx := context.Background()
	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, "rich-mail-test", domain.NewSecretHandle([]byte("test-mail-password")))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := app.CreateAccount(ctx, application.CreateAccountInput{
		Name: "서식 메일", Email: "me@corp.local",
		POP3Host: "127.0.0.1", POP3Security: "none", POP3Username: "me", POP3SecretRef: string(ref),
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc.ID
}

func richMailElements(t *testing.T, page, tag string) []*html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

func richMailAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func assertRichMailPreview(t *testing.T, page, bodyHTML string) {
	t.Helper()
	for _, frame := range richMailElements(t, page, "iframe") {
		if title, _ := richMailAttr(frame, "title"); title != "서식 메일 미리보기" {
			continue
		}
		if sandbox, present := richMailAttr(frame, "sandbox"); !present || sandbox != "" {
			t.Fatalf("mail preview must have an empty sandbox, got %q (present=%v)", sandbox, present)
		}
		srcdoc, _ := richMailAttr(frame, "srcdoc")
		if !strings.Contains(srcdoc, bodyHTML) {
			t.Fatalf("preview does not contain the saved HTML: %q", srcdoc)
		}
		if strings.Contains(srcdoc, "<script") || strings.Contains(srcdoc, "onerror") || strings.Contains(srcdoc, "javascript:") {
			t.Fatalf("active content survived into preview: %q", srcdoc)
		}
		return
	}
	t.Fatal("no sandboxed rich mail preview found")
}

// Exercise the user's complete path, including the actual SMTP payload. A
// preview alone cannot catch a regression that still sends only text/plain.
func TestRichMailComposeEditApproveAndSend(t *testing.T) {
	app, _ := newTestApp(t)
	smtp := &richMailSMTP{}
	app.SMTP = smtp
	accountID := richMailAccount(t, app)
	h := New(app, "").Handler()
	ctx := context.Background()
	unsafeHTML := `<h2 style="color:#2563eb">주간 소식</h2><p>안녕하세요 <strong>팀 여러분</strong> &amp; 동료들</p><script>alert('mail-xss')</script><img src="x" onerror="alert('mail-xss')"><a href="javascript:alert('mail-xss')">링크</a>`
	rec := do(t, h, http.MethodPost, "/ui/compose", url.Values{
		"account_id": {accountID}, "to": {"team@corp.local"}, "subject": {"주간 안내"},
		"body_format": {"html"}, "body_html": {unsafeHTML}, "body": {"outdated text"},
	}, nil)
	draftURL := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(draftURL, "/ui/drafts/") {
		t.Fatalf("compose code=%d location=%q body=%s", rec.Code, draftURL, rec.Body.String())
	}
	draftID := strings.TrimPrefix(draftURL, "/ui/drafts/")
	dv, err := app.GetDraft(ctx, draftID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dv.Version.BodyHTML, "<strong>팀 여러분</strong>") || !strings.Contains(dv.Version.BodyHTML, "color:") {
		t.Fatalf("formatting was lost: %q", dv.Version.BodyHTML)
	}
	if strings.Contains(dv.Version.BodyHTML, "mail-xss") || strings.Contains(dv.Version.BodyHTML, "onerror") {
		t.Fatalf("unsafe HTML was persisted: %q", dv.Version.BodyHTML)
	}
	if !strings.Contains(dv.Version.BodyText, "팀 여러분") || strings.Contains(dv.Version.BodyText, "outdated text") {
		t.Fatalf("plain alternative does not match HTML: %q", dv.Version.BodyText)
	}

	rec = do(t, h, http.MethodGet, draftURL, nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-mail-editor") {
		t.Fatalf("rich draft editor missing: code=%d", rec.Code)
	}
	seedFound := false
	for _, n := range richMailElements(t, rec.Body.String(), "textarea") {
		if _, ok := richMailAttr(n, "data-html-source"); !ok {
			continue
		}
		var seed strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.TextNode {
				t.Fatal("editor HTML seed must be escaped text")
			}
			seed.WriteString(c.Data)
		}
		if seed.String() != dv.Version.BodyHTML {
			t.Fatalf("editor seed does not match saved HTML: %q", seed.String())
		}
		seedFound = true
	}
	if !seedFound {
		t.Fatal("HTML source seed missing")
	}

	updated := `<h2 style="color:#2563eb">수정된 소식</h2><p><strong>금요일</strong>에 뵙겠습니다.</p><ul><li>일정 확인</li></ul>`
	rec = do(t, h, http.MethodPost, draftURL, url.Values{
		"to": {"team@corp.local"}, "bcc": {"archive@corp.local"}, "subject": {"수정된 주간 안내"},
		"body_format": {"html"}, "body_html": {updated},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit code=%d body=%s", rec.Code, rec.Body.String())
	}
	dv, err = app.GetDraft(ctx, draftID)
	if err != nil || dv.Version.Version != 2 {
		t.Fatalf("updated draft=%+v err=%v", dv, err)
	}
	rec = do(t, h, http.MethodGet, draftURL+"/send", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview code=%d body=%s", rec.Code, rec.Body.String())
	}
	assertRichMailPreview(t, rec.Body.String(), dv.Version.BodyHTML)
	rec = do(t, h, http.MethodPost, draftURL+"/send", url.Values{"action": {"approve"}}, nil)
	token := extractToken(rec.Body.String())
	if rec.Code != http.StatusOK || token == "" {
		t.Fatalf("approval code=%d token missing=%v", rec.Code, token == "")
	}
	assertRichMailPreview(t, rec.Body.String(), dv.Version.BodyHTML)
	rec = do(t, h, http.MethodPost, draftURL+"/send", url.Values{"action": {"confirm"}, "token": {token}}, nil)
	if rec.Code != http.StatusOK || smtp.sends != 1 {
		t.Fatalf("send code=%d SMTP sends=%d body=%s", rec.Code, smtp.sends, rec.Body.String())
	}
	if smtp.opts.AuthMethod != "none" || smtp.opts.Password != nil {
		t.Fatal("rich mail must preserve unauthenticated internal SMTP relay settings")
	}
	msg, err := mail.ReadMessage(bytes.NewReader(smtp.raw))
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("outgoing MIME type=%q err=%v", mediaType, err)
	}
	if msg.Header.Get("Bcc") != "" {
		t.Fatal("Bcc must not appear in the message headers")
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for i, want := range []struct{ kind, body string }{{"text/plain", dv.Version.BodyText}, {"text/html", dv.Version.BodyHTML}} {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		kind, p, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || kind != want.kind || !strings.EqualFold(p["charset"], "utf-8") {
			t.Fatalf("part %d content type=%q params=%v err=%v", i, kind, p, err)
		}
		body, err := io.ReadAll(part)
		if err != nil || strings.TrimSpace(strings.ReplaceAll(string(body), "\r\n", "\n")) != strings.TrimSpace(want.body) {
			t.Fatalf("part %d does not match approved body: %q err=%v", i, body, err)
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("expected exactly two alternatives, got %v", err)
	}
}

func TestRichMailSwitchToPlainDropsPreviousHTML(t *testing.T) {
	app, _ := newTestApp(t)
	accountID := richMailAccount(t, app)
	h := New(app, "").Handler()
	rec := do(t, h, http.MethodPost, "/ui/compose", url.Values{
		"account_id": {accountID}, "to": {"team@corp.local"}, "subject": {"본문 변경"},
		"body_format": {"html"}, "body_html": {`<p><strong>이전 내용</strong></p>`},
	}, nil)
	draftURL := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(draftURL, "/ui/drafts/") {
		t.Fatalf("compose code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, draftURL, url.Values{
		"to": {"team@corp.local"}, "subject": {"일반 텍스트로 변경"},
		"body_format": {"plain"}, "body": {"새 일반 텍스트 본문"},
		"body_html": {`<p>이전 내용이 남아 있어도 발송하면 안 됨</p>`},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("plain edit code=%d body=%s", rec.Code, rec.Body.String())
	}
	dv, err := app.GetDraft(context.Background(), strings.TrimPrefix(draftURL, "/ui/drafts/"))
	if err != nil || dv.Version.BodyHTML != "" || dv.Version.BodyText != "새 일반 텍스트 본문" {
		t.Fatalf("plain edit retained stale HTML: draft=%+v err=%v", dv, err)
	}
	rec = do(t, h, http.MethodGet, draftURL+"/send", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "새 일반 텍스트 본문") || strings.Contains(rec.Body.String(), "서식 메일 미리보기") {
		t.Fatalf("plain preview code=%d body=%s", rec.Code, rec.Body.String())
	}
}
