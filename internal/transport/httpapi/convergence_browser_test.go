package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/receivedhtml"
	"postra/internal/transport/spa"
)

func TestSPAConvergenceBrowser(t *testing.T) {
	if os.Getenv("POSTRA_SPA_BROWSER_TEST") != "1" {
		t.Skip("set POSTRA_SPA_BROWSER_TEST=1 after building embedded SPA assets")
	}
	const login, password, otherLogin, otherPassword = "convergence-admin", "convergence-fixture-password-2026", "convergence-member", "convergence-other-fixture-2026"
	app := browserTestApp(t, true)
	provider := newBrowserOIDCProvider(t)
	var imageRequests atomic.Int32
	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageRequests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl6eN0AAAAASUVORK5CYII=")
		_, _ = w.Write(data)
	}))
	t.Cleanup(imageServer.Close)
	mux := http.NewServeMux()
	handler := New(app, "").Handler()
	mux.Handle("/auth/", handler)
	mux.Handle("/api/", handler)
	mux.Handle("/tracking/", handler)
	mux.Handle("/app/", spa.Handler())
	mux.Handle("/app", spa.Handler())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	ctx := context.Background()
	if err := app.Store.UpsertSettings(ctx, map[string]string{application.SettingOIDCIssuer: provider.server.URL, application.SettingOIDCClientID: "browser-client", application.SettingOIDCRedirectURL: server.URL + "/auth/oidc/callback", application.SettingOIDCAutoLogin: "false", application.SettingOIDCAutoProvision: "true"}); err != nil {
		t.Fatal(err)
	}
	other := &domain.User{ID: "convergence-member", LoginID: otherLogin, DisplayName: "개인 사용자", Email: "member@corp.local", Status: domain.UserActive, Role: domain.RoleUser, AuthProvider: "local"}
	if err := app.Store.CreateUser(ctx, other, ""); err != nil {
		t.Fatal(err)
	}
	adminCtx := application.WithPrincipal(ctx, domain.Principal{UserID: application.DefaultUserID, Role: domain.RoleAdmin})
	if err := app.AdminResetPassword(adminCtx, other.ID, otherPassword); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct{ owner, id, subject string }{{application.DefaultUserID, "convergence-own", "정책 확인용 내 메일"}, {other.ID, "convergence-private", "개인 사용자 비밀 메일"}} {
		account := &domain.MailAccount{ID: "acc-" + seed.id, UserID: seed.owner, Name: "검증 계정", Email: seed.id + "@corp.local", Status: domain.AccountActive}
		if err := app.Store.CreateAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
		message := &domain.Message{ID: seed.id, UserID: seed.owner, AccountID: account.ID, UIDL: seed.id, RawHash: seed.id, From: domain.Address{Email: "sender@corp.local"}, Subject: seed.subject, Date: time.Now().Unix()}
		body := &domain.MessageBody{MessageID: seed.id, TextBody: "각자의 메일 본문입니다.", HTMLSanitized: receivedhtml.Sanitize(`<h2>안전한 본문</h2><p>각자의 메일 본문입니다.</p><img src="` + imageServer.URL + `/photo.png" width="400">`)}
		if err := app.Store.InsertMessage(ctx, message, body, nil); err != nil {
			t.Fatal(err)
		}
	}
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(commandCtx, "node", "../../../web/e2e/convergence.cjs")
	command.Env = append(os.Environ(), "POSTRA_TEST_URL="+server.URL, "POSTRA_TEST_OIDC_URL="+provider.server.URL, "POSTRA_TEST_IMAGE_URL="+imageServer.URL, "POSTRA_TEST_LOGIN="+login, "POSTRA_TEST_PASSWORD="+password, "POSTRA_TEST_OTHER_LOGIN="+otherLogin, "POSTRA_TEST_OTHER_PASSWORD="+otherPassword)
	output, err := command.CombinedOutput()
	safe := strings.NewReplacer(password, "[REDACTED]", otherPassword, "[REDACTED]").Replace(string(output))
	if err != nil {
		t.Fatalf("convergence browser: %v\n%s", err, safe)
	}
	t.Log(safe)
	if imageRequests.Load() != 1 {
		t.Fatalf("only explicit one-time consent may load the remote image, got %d requests", imageRequests.Load())
	}
	select {
	case redirect := <-provider.redirect:
		if redirect != server.URL+"/auth/oidc/callback" {
			t.Fatal("new OIDC callback redirect_uri mismatch")
		}
	default:
		t.Fatal("real browser did not exchange an OIDC code")
	}
}
