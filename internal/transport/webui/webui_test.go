package webui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

type fakeAI struct{}

func (fakeAI) Generate(context.Context, domain.GenerationRequest) (domain.GenerationResult, error) {
	return domain.GenerationResult{
		Text:  `{"summary":"중요한 테스트 메일입니다.","requests":[],"dates":[],"confidence":0.9}`,
		Model: "fake",
	}, nil
}
func (fakeAI) Embed(context.Context, domain.EmbeddingRequest) (domain.EmbeddingResult, error) {
	return domain.EmbeddingResult{Vectors: [][]float32{{1, 0}}, Model: "fake"}, nil
}

type fakeInboundDialer struct{}
type fakeInboundSession struct{}

func (fakeInboundDialer) Dial(context.Context, domain.InboundDialOptions) (domain.InboundSession, error) {
	return fakeInboundSession{}, nil
}
func (fakeInboundSession) List(context.Context) ([]domain.RemoteMessage, error) { return nil, nil }
func (fakeInboundSession) UIDL(context.Context) ([]domain.RemoteMessage, error) { return nil, nil }
func (fakeInboundSession) Retrieve(context.Context, int) (io.ReadCloser, error) { return nil, nil }
func (fakeInboundSession) Top(context.Context, int, int) (io.ReadCloser, error) { return nil, nil }
func (fakeInboundSession) Delete(context.Context, int) error                    { return nil }
func (fakeInboundSession) Quit(context.Context) error                           { return nil }
func (fakeInboundSession) Close() error                                         { return nil }

func newTestApp(t *testing.T) (*application.App, *fakeSMTP) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.AllowInsecureMail = true
	cfg.AllowPrivateHosts = true
	cfg.Auth.Enabled = false

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
	app.POP3 = fakeInboundDialer{}
	app.IMAP = fakeInboundDialer{}
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

func TestLocalLoginSessionAndLogout(t *testing.T) {
	app, _ := newTestApp(t)
	app.Cfg.Auth.Enabled = true
	if _, err := app.SetupInitialAdmin(context.Background(), "admin", "Administrator", "a-secure-password"); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()

	rec := do(t, h, http.MethodGet, "/ui/", nil, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("anonymous request: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(t, h, http.MethodPost, "/ui/login", url.Values{
		"login_id": {"admin"}, "password": {"a-secure-password"},
	}, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "./" {
		t.Fatalf("login: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode ||
		sessionCookie.Path != "/" {
		t.Fatalf("secure session cookie was not issued: %+v", sessionCookie)
	}
	rec = do(t, h, http.MethodGet, "/ui/", nil, sessionCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated request: code=%d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/ui/login", nil, sessionCookie)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "./" {
		t.Fatalf("authenticated login page re-entry: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	req := httptest.NewRequest(http.MethodPost, "/ui/logout", nil)
	req.AddCookie(sessionCookie)
	req.Header.Set("Origin", "http://example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("logout: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/ui/", nil, sessionCookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("logged-out cookie remained valid: code=%d", rec.Code)
	}
}

func TestLoginRedirectWithRealCookieJar(t *testing.T) {
	app, _ := newTestApp(t)
	app.Cfg.Auth.Enabled = true
	if _, err := app.SetupInitialAdmin(context.Background(), "admin", "Administrator", "a-secure-password"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(app, "").Handler())
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Jar = jar
	resp, err := client.PostForm(server.URL+"/ui/login", url.Values{
		"login_id": {"admin"}, "password": {"a-secure-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/ui/" {
		t.Fatalf("login redirect ended at %s with %d: %s", resp.Request.URL.Path, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "Postra 로그인") {
		t.Fatal("successful login redirected back to the login page")
	}
}

func TestLoginBehindPathRewritingProxy(t *testing.T) {
	app, _ := newTestApp(t)
	app.Cfg.Auth.Enabled = true
	if _, err := app.SetupInitialAdmin(context.Background(), "admin", "Administrator", "a-secure-password"); err != nil {
		t.Fatal(err)
	}
	backend := New(app, "").Handler()
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			r.URL.Path = "/ui/login"
		case "/":
			r.URL.Path = "/ui/"
		}
		backend.ServeHTTP(w, r)
	})
	server := httptest.NewServer(proxy)
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := server.Client()
	client.Jar = jar

	resp, err := client.PostForm(server.URL+"/login", url.Values{
		"login_id": {"admin"}, "password": {"a-secure-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/" {
		t.Fatalf("proxied login ended at %s with %d", resp.Request.URL.Path, resp.StatusCode)
	}
}

func TestSearchPagination(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := context.Background()
	// Seed one more than a page so a second page (and "더 보기") exists.
	for i := 0; i < searchPageSize+1; i++ {
		m := &domain.Message{
			ID: fmt.Sprintf("msg_%03d", i), UserID: application.DefaultUserID, AccountID: "acc_x",
			Subject: fmt.Sprintf("page item %d", i), From: domain.Address{Email: "a@x"},
			Date: int64(1000 + i), RawHash: fmt.Sprintf("h%03d", i), RawURI: "mem://x",
		}
		if err := app.Store.InsertMessage(ctx, m, nil, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	h := New(app, "").Handler()

	// Page 1: full page + a "more" link.
	rec := do(t, h, "GET", "/ui/?q=page", nil, nil)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("search code=%d", rec.Code)
	}
	if !strings.Contains(body, "더 보기") {
		t.Fatal("expected a '더 보기' pagination link on page 1")
	}
	if n := strings.Count(body, "/ui/messages/"); n != searchPageSize {
		t.Fatalf("page 1 row count = %d, want %d", n, searchPageSize)
	}

	// Follow the cursor link → page 2 with the remaining item, no further link.
	// html/template escapes & as &amp; in the href; a browser decodes it back.
	i := strings.Index(body, `href="/ui/?q=page&amp;cursor=`)
	if i < 0 {
		t.Fatal("no cursor link found")
	}
	rest := body[i+len(`href="`):]
	next := strings.ReplaceAll(rest[:strings.IndexByte(rest, '"')], "&amp;", "&")
	rec2 := do(t, h, "GET", next, nil, nil)
	body2 := rec2.Body.String()
	if strings.Count(body2, "/ui/messages/") != 1 {
		t.Fatalf("page 2 row count = %d, want 1", strings.Count(body2, "/ui/messages/"))
	}
	if strings.Contains(body2, "더 보기") {
		t.Fatal("page 2 should not offer a further page")
	}
}

func TestAuthGate(t *testing.T) {
	app, _ := newTestApp(t)
	h := New(app, "sekret").Handler()

	// No cookie → redirect to login.
	rec := do(t, h, "GET", "/ui/", nil, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("expected redirect to login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Wrong token login → 401.
	rec = do(t, h, "POST", "/ui/login", url.Values{"token": {"nope"}}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login code=%d", rec.Code)
	}

	// Correct token login sets the cookie.
	rec = do(t, h, "POST", "/ui/login", url.Values{"token": {"sekret"}}, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "./" {
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

// TestOnboardingComposeAndEdit covers the no-CLI product journey:
// browser credential registration → account creation → compose → edit.
func TestOnboardingComposeAndEdit(t *testing.T) {
	app, _ := newTestApp(t)
	h := New(app, "").Handler()

	rec := do(t, h, "GET", "/ui/", nil, nil)
	if !strings.Contains(rec.Body.String(), "첫 계정 연결") {
		t.Fatal("empty inbox should guide the user to account onboarding")
	}
	const password = "not-visible-in-html"
	rec = do(t, h, "POST", "/ui/accounts", url.Values{
		"name":             {"업무"},
		"email":            {"me@example.test"},
		"inbound_protocol": {"pop3"},
		"inbound_host":     {"127.0.0.1"},
		"inbound_security": {"none"},
		"inbound_username": {"me"},
		"password":         {password},
		"smtp_host":        {"127.0.0.1"},
		"smtp_security":    {"none"},
		"smtp_username":    {"me"},
	}, nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/ui/accounts/") {
		t.Fatalf("account create: code=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), password) || strings.Contains(rec.Header().Get("Location"), password) {
		t.Fatal("credential leaked into the HTTP response")
	}
	accounts, err := app.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	rec = do(t, h, "POST", "/ui/accounts/"+accounts[0].ID, url.Values{
		"name": {"업무 수정"}, "email": {"changed@example.test"}, "status": {"active"},
		"inbound_protocol": {"pop3"}, "inbound_host": {"127.0.0.1"}, "inbound_port": {"110"},
		"inbound_security": {"none"}, "inbound_username": {"changed"},
		"smtp_host": {"127.0.0.1"}, "smtp_port": {"587"}, "smtp_security": {"none"},
		"smtp_auth": {"none"}, "smtp_username": {"changed"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("account update code=%d body=%s", rec.Code, rec.Body.String())
	}
	updated, err := app.GetAccount(context.Background(), accounts[0].ID)
	if err != nil || updated.Name != "업무 수정" || updated.Email != "changed@example.test" {
		t.Fatalf("updated account=%+v err=%v", updated, err)
	}
	rec = do(t, h, "POST", "/ui/accounts/"+accounts[0].ID+"/test", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "연결 진단") ||
		!strings.Contains(rec.Body.String(), "정상") {
		t.Fatalf("connection diagnostics code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "POST", "/ui/accounts/"+accounts[0].ID+"/sync", nil, nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/ui/jobs/") {
		t.Fatalf("sync start code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	jobURL := rec.Header().Get("Location")
	for i := 0; i < 20; i++ {
		rec = do(t, h, "GET", jobURL, nil, nil)
		if strings.Contains(rec.Body.String(), "succeeded") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "succeeded") ||
		!strings.Contains(rec.Body.String(), "받은편지함 보기") {
		t.Fatalf("sync status code=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, "POST", "/ui/compose", url.Values{
		"account_id": {accounts[0].ID}, "to": {"bob@example.test"},
		"subject": {"초기 제목"}, "body": {"초기 본문"},
	}, nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/ui/drafts/") {
		t.Fatalf("compose: code=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	draftURL := rec.Header().Get("Location")
	draftID := strings.TrimPrefix(draftURL, "/ui/drafts/")
	rec = do(t, h, "POST", draftURL, url.Values{
		"to": {"bob@example.test"}, "subject": {"수정 제목"}, "body": {"수정 본문"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("draft update code=%d body=%s", rec.Code, rec.Body.String())
	}
	dv, err := app.GetDraft(context.Background(), draftID)
	if err != nil || dv.Version.Subject != "수정 제목" || dv.Version.BodyText != "수정 본문" || dv.Draft.CurrentVersion != 2 {
		t.Fatalf("updated draft=%+v err=%v", dv, err)
	}
	rec = do(t, h, "POST", draftURL+"/delete", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("draft delete code=%d", rec.Code)
	}
	rec = do(t, h, "POST", "/ui/accounts/"+accounts[0].ID+"/delete",
		url.Values{"confirm": {"changed@example.test"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("account delete code=%d body=%s", rec.Code, rec.Body.String())
	}
	if listed, _ := app.ListAccounts(context.Background()); len(listed) != 0 {
		t.Fatalf("deleted account remained listed: %+v", listed)
	}
}

func TestMessageAnalysisAndAttachmentDownload(t *testing.T) {
	app, _ := newTestApp(t)
	app.AI = fakeAI{}
	ctx := context.Background()
	uri, hash, size, err := app.Objects.Put("attachment", strings.NewReader("safe content"))
	if err != nil {
		t.Fatal(err)
	}
	m := &domain.Message{
		ID: "msg_tools", UserID: application.DefaultUserID, AccountID: "acc_x",
		Subject: "도구 테스트", From: domain.Address{Email: "a@example.test"},
		Date: 1000, RawHash: "raw_tools", RawURI: "local://raw/not-used", HasAttachments: true,
	}
	body := &domain.MessageBody{MessageID: m.ID, TextBody: "중요한 요청입니다."}
	at := domain.Attachment{
		ID: "att_safe", MessageID: m.ID, Name: "safe.txt", MIMEType: "text/plain",
		Size: size, Hash: hash, StorageURI: uri, ScanStatus: domain.ScanClean,
	}
	if err := app.Store.InsertMessage(ctx, m, body, []domain.Attachment{at}); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()

	rec := do(t, h, "GET", "/ui/messages/msg_tools/attachments/att_safe", nil, nil)
	if rec.Code != 200 || rec.Body.String() != "safe content" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "safe.txt") {
		t.Fatalf("download code=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	rec = do(t, h, "POST", "/ui/messages/msg_tools/analyze", url.Values{"type": {"summarize"}}, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "중요한 테스트 메일") {
		t.Fatalf("analysis code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStaticAssets(t *testing.T) {
	app, _ := newTestApp(t)
	h := New(app, "").Handler()

	for _, path := range []string{"/favicon.ico", "/favicon.png", "/logo.png", "/ui/static/logo.png", "/ui/static/favicon.png"} {
		rec := do(t, h, "GET", path, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("path %s returned status %d", path, rec.Code)
		}
		if len(rec.Body.Bytes()) == 0 {
			t.Fatalf("path %s returned empty body", path)
		}
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
