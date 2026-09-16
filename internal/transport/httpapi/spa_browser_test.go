package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/adapters/secretstore"
	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
	"postra/internal/platform/receivedhtml"
	"postra/internal/transport/spa"
)

type spaBrowserSMTP struct{ sent atomic.Int32 }

func (*spaBrowserSMTP) TestConnection(context.Context, domain.SMTPSendOptions) (*domain.ConnDiagnostics, error) {
	return &domain.ConnDiagnostics{Target: "smtp", OK: true}, nil
}

func (s *spaBrowserSMTP) Send(_ context.Context, _ domain.SMTPSendOptions, _ domain.Envelope, message io.Reader) (domain.SendReceipt, error) {
	if _, err := io.Copy(io.Discard, message); err != nil {
		return domain.SendReceipt{}, err
	}
	s.sent.Add(1)
	return domain.SendReceipt{ServerResponse: "250 accepted by in-process browser-test fake"}, nil
}

type spaBrowserInbound struct{}

func (s spaBrowserInbound) Dial(context.Context, domain.POP3DialOptions) (domain.POP3Session, error) {
	return s, nil
}
func (spaBrowserInbound) List(context.Context) ([]domain.RemoteMessage, error) { return nil, nil }
func (spaBrowserInbound) UIDL(context.Context) ([]domain.RemoteMessage, error) { return nil, nil }
func (spaBrowserInbound) Retrieve(context.Context, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (s spaBrowserInbound) Top(ctx context.Context, n, _ int) (io.ReadCloser, error) {
	return s.Retrieve(ctx, n)
}
func (spaBrowserInbound) Delete(context.Context, int) error { return nil }
func (spaBrowserInbound) Quit(context.Context) error        { return nil }
func (spaBrowserInbound) Close() error                      { return nil }

type spaBrowserAI struct{}

func (spaBrowserAI) Generate(_ context.Context, req domain.GenerationRequest) (domain.GenerationResult, error) {
	result := `{"summary":"프로젝트 킥오프를 앞두고 회의 자료 검토와 참석 확인을 요청했습니다.","requests":["회의 자료 검토","참석 확인"],"dates":[],"confidence":0.96}`
	switch req.Task {
	case "draft_reply", "compose", "rewrite":
		result = `{"subject":"회의 자료 검토 회신","body":"안녕하세요. 공유해 주신 회의 자료를 확인했습니다. 검토 후 의견을 전달드리겠습니다."}`
	case "triage":
		result = `{"sender_intent":"회의 준비 및 자료 검토 요청","priority":"high","reply_required":true,"deadline":null,"business_risks":[],"recommended_next_action":"회의 자료를 검토하고 참석 여부를 회신하세요.","confidence":0.96}`
	case "classify":
		result = `{"category":"work","importance":"high","reason":"프로젝트 회의 준비 요청","confidence":0.96}`
	case "question_answer", "qa":
		result = `{"answer":"공유된 회의 자료를 검토하고 참석 여부를 회신하면 됩니다.","evidence_message_ids":["msg_spa_welcome"],"confidence":0.96}`
	case "action_cards":
		result = `{"cards":[{"type":"todo","title":"회의 자료 검토","detail":"공유된 회의 자료를 확인합니다.","due":null,"assignee":null,"confidence":0.95}]}`
	case "daily_digest", "digest":
		result = `{"headline":"프로젝트 회의 준비와 보안 점검을 확인하세요.","needs_reply":[{"message_id":"msg_spa_welcome","who":"김민수","what":"회의 자료 검토와 참석 확인"}],"deadlines":[],"fyi":["지난주 업무 공유 완료"],"volume_note":"내 메일 3건"}`
	case "smart_reply":
		result = `{"suggestions":["공유해 주신 자료를 확인했습니다. 검토 후 의견을 전달드리겠습니다.","자료 검토 시 우선 확인할 항목을 알려 주시겠어요?","자료를 확인하고 참석 여부를 회신드리겠습니다."]}`
	}
	return domain.GenerationResult{Text: result, Model: "in-process-browser-fake", InputHash: "browser-fixture"}, nil
}

func (spaBrowserAI) Embed(_ context.Context, req domain.EmbeddingRequest) (domain.EmbeddingResult, error) {
	result := domain.EmbeddingResult{Model: "in-process-browser-fake"}
	for range req.Input {
		result.Vectors = append(result.Vectors, []float32{0.25, 0.5, 0.75})
	}
	return result, nil
}

// TestSPABrowser is optional because it needs the built Vite assets and a local
// Playwright browser. The real Go routes/auth/storage run against temp data;
// every mail and AI adapter is an in-process fake, so nothing leaves the test.
func TestSPABrowser(t *testing.T) {
	if os.Getenv("POSTRA_SPA_BROWSER_TEST") != "1" {
		t.Skip("set POSTRA_SPA_BROWSER_TEST=1 after building web assets and installing Playwright")
	}
	const (
		login          = "spa-browser-admin"
		password       = "browser-fixture-only-2026"
		email          = "hong@corp.local"
		accountID      = "acc_spa_browser"
		messageID      = "msg_spa_welcome"
		otherMessageID = "msg_spa_private"
		otherLogin     = "other-browser-member"
		otherPassword  = "other-browser-fixture-only-2026"
		otherEmail     = "other@corp.local"
		privateSubject = "다른 사용자 비공개 인사 평가"
		draftID        = "drf_spa_saved"
		actionID       = "act_spa_review"
	)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Auth.Enabled = true
	cfg.Auth.BootstrapAdmin = login
	cfg.Auth.BootstrapPassword = password
	cfg.AllowPrivateHosts, cfg.AllowInsecureMail = true, true
	kek, err := crypto.LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := persistence.Open(filepath.Join(dir, "spa-browser.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.EnableEncryption(kek)
	local, err := objectstore.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	objects := objectstore.NewEncrypted(local, kek)
	smtp := &spaBrowserSMTP{}
	app, err := application.New(cfg, store, objects, secretstore.NewLocal(dir, kek), spaBrowserInbound{}, smtp, spaBrowserAI{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)
	ctx := context.Background()
	u, err := store.GetUser(ctx, application.DefaultUserID)
	if err != nil {
		t.Fatal(err)
	}
	u.Email, u.DisplayName = email, "홍길동"
	if err := store.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, "browser fake inbound", domain.NewSecretHandle([]byte("in-process-only")))
	if err != nil {
		t.Fatal(err)
	}
	for _, acc := range []*domain.MailAccount{
		{ID: accountID, UserID: u.ID, Name: "회사 메일", Email: email, Status: domain.AccountActive, InboundProtocol: "pop3", POP3Host: "127.0.0.1", POP3Port: 110, POP3Security: domain.SecurityNone, POP3Username: email, POP3Secret: ref, SMTPHost: "127.0.0.1", SMTPPort: 25, SMTPSecurity: domain.SecurityNone, SMTPAuth: "none"},
	} {
		if err := store.CreateAccount(ctx, acc); err != nil {
			t.Fatal(err)
		}
	}
	other := &domain.User{ID: "usr_spa_other", LoginID: otherLogin, DisplayName: "다른 사용자", Email: otherEmail, Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "local"}
	if err := store.CreateUser(ctx, other, ""); err != nil {
		t.Fatal(err)
	}
	adminCtx := application.WithPrincipal(ctx, domain.Principal{UserID: u.ID, LoginID: login, Role: domain.RoleAdmin, AuthMethod: "local"})
	if err := app.AdminResetPassword(adminCtx, other.ID, otherPassword); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, &domain.MailAccount{ID: "acc_spa_other", UserID: other.ID, Name: "비공개 계정", Email: other.Email, Status: domain.AccountActive}); err != nil {
		t.Fatal(err)
	}
	// These local sites stand in for clicked web links. They are never fetched
	// until a human-equivalent click, and have no application cookies or data.
	linkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/landing" {
			t.Errorf("unexpected link fixture request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Referer() != "" {
			t.Error("clicked mail link leaked a Referer")
		}
		if _, err := r.Cookie("postra_session"); err == nil {
			t.Error("clicked mail link leaked the workspace session")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><title>링크 도착</title><link rel="icon" href="data:,"><p>안전한 링크 테스트</p><script>document.title='LINK_OPENED'</script>`)
	})
	linkHTTP := httptest.NewServer(linkHandler)
	linkHTTPS := httptest.NewTLSServer(linkHandler)
	t.Cleanup(linkHTTP.Close)
	t.Cleanup(linkHTTPS.Close)
	linkHTTPURL := strings.Replace(linkHTTP.URL, "127.0.0.1", "localhost", 1)
	linkHTTPSURL := strings.Replace(linkHTTPS.URL, "127.0.0.1", "localhost", 1)
	now := time.Now()
	for i, seed := range []struct {
		id, userID, accountID, subject, text, sender string
		important, archived                          bool
	}{
		{messageID, u.ID, accountID, "프로젝트 킥오프 일정 공유", "안녕하세요. 프로젝트 킥오프에 앞서 공유된 회의 자료를 검토하고 참석 여부를 회신해 주세요. 감사합니다.", "김민수", false, false},
		{"msg_spa_important", u.ID, accountID, "3분기 보안 점검 결과 확인 요청", "보안 점검 결과를 공유합니다. 조치 항목을 확인하고 담당자를 지정해 주세요.", "이지은", true, false},
		{"msg_spa_archived", u.ID, accountID, "지난주 진행 현황 공유", "지난주 계획한 업무를 완료했습니다. 검토해 주셔서 감사합니다.", "박수진", false, true},
		{otherMessageID, other.ID, "acc_spa_other", privateSubject, "다른 사용자의 인사 평가 자료입니다. 관리자에게도 노출되어서는 안 됩니다.", "인사 담당", false, false},
	} {
		if seed.id == "msg_spa_important" {
			seed.text += "\n" + linkHTTPURL + "/landing\n" + linkHTTPSURL + "/landing\nhelp@corp.local"
		}
		recipient := email
		if seed.userID == other.ID {
			recipient = otherEmail
		}
		raw := fmt.Sprintf("From: sender@corp.local\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s", recipient, seed.subject, seed.text)
		uri, hash, size, err := objects.Put("raw", strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		m := &domain.Message{ID: seed.id, UserID: seed.userID, AccountID: seed.accountID, UIDL: seed.id, MessageID: "<" + seed.id + "@corp.local>", Subject: seed.subject, From: domain.Address{Name: seed.sender, Email: "sender@corp.local"}, To: []domain.Address{{Email: recipient}}, Date: now.Add(-time.Duration(i) * time.Hour).Unix(), Size: size, RawHash: hash, RawURI: uri, ThreadID: "thr_" + seed.id, IsImportant: seed.important, IsArchived: seed.archived}
		body := &domain.MessageBody{MessageID: seed.id, TextBody: seed.text, HTMLSanitized: "<h2>" + seed.subject + "</h2><p>" + seed.text + "</p>", Charset: "utf-8"}
		if seed.id == messageID {
			body.HTMLSanitized = receivedhtml.Sanitize(body.HTMLSanitized + `<a href="` + linkHTTPURL + `/landing" target="_top" rel="opener">HTTP 안내</a><a href="` + linkHTTPSURL + `/landing">HTTPS 안내</a><a href="mailto:help@corp.local?subject=Hello">메일 문의</a><a href="javascript:alert('unsafe')">위험 링크</a><img src="` + linkHTTPURL + `/tracking.png"><form action="` + linkHTTPURL + `/form"><button>전송 금지</button></form><script>parent.location='/unsafe'</script>`)
		} else if seed.id == "msg_spa_important" {
			body.HTMLSanitized = ""
		}
		if err := store.InsertMessage(ctx, m, body, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateDraft(ctx, &domain.Draft{ID: draftID, UserID: u.ID, AccountID: accountID, Kind: domain.DraftNew, Status: domain.DraftOpen}, &domain.DraftVersion{Subject: "작성 중인 안내 메일", BodyText: "검토 후 공유드리겠습니다.", BodyHTML: "<p>검토 후 <strong>공유드리겠습니다.</strong></p>", To: []domain.Address{{Email: "colleague@corp.local"}}, Author: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMessageCollab(ctx, &domain.MessageCollab{MessageID: messageID, UserID: u.ID, Assignee: "홍길동", Status: domain.CollabOpen, UpdatedBy: login}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateActionCard(ctx, &domain.ActionCard{ID: actionID, UserID: u.ID, MessageID: messageID, Type: "todo", Title: "회의 자료 검토", Detail: "킥오프 자료를 검토하고 의견을 회신합니다.", Status: domain.ActionCardPending, Confidence: 0.96}); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	apiHandler := New(app, "").Handler()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/auth/", apiHandler)
	mux.Handle("/tracking/", apiHandler)
	mux.Handle("/momento/", apiHandler)
	appHandler := spa.Handler()
	mux.Handle("/app", appHandler)
	mux.Handle("/app/", appHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "node", "../../../web/tests/real-server.cjs")
	cmd.Env = append(os.Environ(),
		"POSTRA_TEST_URL="+srv.URL, "POSTRA_TEST_LOGIN="+login, "POSTRA_TEST_PASSWORD="+password,
		"POSTRA_TEST_EMAIL="+email, "POSTRA_TEST_ACCOUNT_ID="+accountID, "POSTRA_TEST_MESSAGE_ID="+messageID,
		"POSTRA_TEST_OTHER_MESSAGE_ID="+otherMessageID, "POSTRA_TEST_PRIVATE_SUBJECT="+privateSubject,
		"POSTRA_TEST_OTHER_LOGIN="+otherLogin, "POSTRA_TEST_OTHER_PASSWORD="+otherPassword, "POSTRA_TEST_OTHER_EMAIL="+otherEmail,
		"POSTRA_TEST_LINK_HTTP="+linkHTTPURL+"/landing", "POSTRA_TEST_LINK_HTTPS="+linkHTTPSURL+"/landing",
		"POSTRA_TEST_DRAFT_ID="+draftID, "POSTRA_TEST_ACTION_ID="+actionID)
	output, err := cmd.CombinedOutput()
	// Even fixture credentials should not appear in failure logs.
	safeOutput := strings.NewReplacer(otherPassword, "[REDACTED]", password, "[REDACTED]").Replace(string(output))
	if err != nil {
		t.Fatalf("SPA browser test: %v\n%s", err, safeOutput)
	}
	if strings.TrimSpace(safeOutput) != "" {
		t.Log(safeOutput)
	}
	if sent := smtp.sent.Load(); sent != 1 {
		t.Fatalf("browser must explicitly approve/send exactly once; fake SMTP received %d sends", sent)
	}
}
