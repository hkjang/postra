package webui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/adapters/secretstore"
	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
)

// fakeSMTP accepts every send so the approval→send flow can be driven fully.
type fakeSMTP struct{ sent int }

func (f *fakeSMTP) TestConnection(context.Context, domain.SMTPSendOptions) (*domain.ConnDiagnostics, error) {
	return &domain.ConnDiagnostics{Target: "smtp", OK: true}, nil
}
func (f *fakeSMTP) Send(context.Context, domain.SMTPSendOptions, domain.Envelope, io.Reader) (domain.SendReceipt, error) {
	f.sent++
	return domain.SendReceipt{ServerResponse: "250 OK"}, nil
}

func newTestApp(t *testing.T) (*application.App, *fakeSMTP) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.AllowInsecureMail = true
	cfg.AllowPrivateHosts = true

	kek, err := crypto.LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := persistence.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.EnableEncryption(kek)
	t.Cleanup(func() { store.Close() })
	local, err := objectstore.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	smtp := &fakeSMTP{}
	app, err := application.New(cfg, store, objectstore.NewEncrypted(local, kek),
		secretstore.NewLocal(dir, kek), nil, smtp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)
	return app, smtp
}

func do(t *testing.T, h http.Handler, method, target string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSearchAndMessage(t *testing.T) {
	app, _ := newTestApp(t)
	h := New(app, "").Handler()

	// Empty search page renders.
	rec := do(t, h, "GET", "/ui/", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "메일 검색") {
		t.Fatalf("search page: code=%d", rec.Code)
	}

	// Unknown message → error page, not a panic or 200.
	rec = do(t, h, "GET", "/ui/messages/msg_missing", nil, nil)
	if rec.Code == 200 {
		t.Fatalf("unknown message should not render 200, got %d", rec.Code)
	}
}

func TestAuthGate(t *testing.T) {
	app, _ := newTestApp(t)
	h := New(app, "sekret").Handler()

	// No cookie → redirect to login.
	rec := do(t, h, "GET", "/ui/", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("expected redirect to login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Wrong token login → 401.
	rec = do(t, h, "POST", "/ui/login", url.Values{"token": {"nope"}}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login code=%d", rec.Code)
	}

	// Correct token login sets the cookie.
	rec = do(t, h, "POST", "/ui/login", url.Values{"token": {"sekret"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login code=%d", rec.Code)
	}
	var sess *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("login did not set session cookie")
	}
	// With the cookie, the search page is reachable.
	rec = do(t, h, "GET", "/ui/", nil, sess)
	if rec.Code != 200 {
		t.Fatalf("authed search code=%d", rec.Code)
	}
}

// TestSendApprovalFlow drives draft → preview → approve → confirm end to end.
func TestSendApprovalFlow(t *testing.T) {
	app, smtp := newTestApp(t)
	ctx := application.WithActor(context.Background(), "test")

	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, "t", domain.NewSecretHandle([]byte("pw")))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := app.CreateAccount(ctx, application.CreateAccountInput{
		Name: "Me", Email: "me@corp.local",
		POP3Host: "127.0.0.1", POP3Security: "none", POP3Username: "me", POP3SecretRef: string(ref),
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	dv, err := app.CreateDraft(ctx, application.CreateDraftInput{
		AccountID: acc.ID, Kind: "new", To: []string{"bob@corp.local"},
		Subject: "안녕", Body: "테스트 본문",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := dv.Draft.ID
	h := New(app, "").Handler()

	// Preview.
	rec := do(t, h, "GET", "/ui/drafts/"+id+"/send", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "bob@corp.local") {
		t.Fatalf("preview code=%d body missing recipient", rec.Code)
	}

	// Approve → token page.
	rec = do(t, h, "POST", "/ui/drafts/"+id+"/send", url.Values{"action": {"approve"}}, nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "발송 확정") {
		t.Fatalf("approve code=%d, expected confirm button", rec.Code)
	}
	tok := extractToken(body)
	if tok == "" {
		t.Fatal("no approval token embedded in confirm page")
	}

	// Confirm → send.
	rec = do(t, h, "POST", "/ui/drafts/"+id+"/send",
		url.Values{"action": {"confirm"}, "token": {tok}}, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "발송 처리됨") {
		t.Fatalf("confirm code=%d, body=%s", rec.Code, rec.Body.String())
	}
	if smtp.sent != 1 {
		t.Fatalf("SMTP send count = %d, want 1", smtp.sent)
	}

	// Re-posting the same draft/version is idempotent (SMTP-007): it returns
	// the original outcome and must NOT trigger a second physical send.
	do(t, h, "POST", "/ui/drafts/"+id+"/send",
		url.Values{"action": {"confirm"}, "token": {tok}}, nil)
	if smtp.sent != 1 {
		t.Fatalf("replay caused a second send (count=%d), want 1", smtp.sent)
	}
}

// extractToken pulls the hidden token field value out of the confirm form.
func extractToken(html string) string {
	const marker = `name="token" value="`
	i := strings.Index(html, marker)
	if i < 0 {
		return ""
	}
	rest := html[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
