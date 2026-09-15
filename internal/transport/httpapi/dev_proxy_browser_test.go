package httpapi

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/receivedhtml"
)

func TestSPADevProxyBrowser(t *testing.T) {
	if os.Getenv("POSTRA_SPA_BROWSER_TEST") != "1" {
		t.Skip("set POSTRA_SPA_BROWSER_TEST=1 to verify the Vite auth/API proxy")
	}
	const login, password = "dev-proxy-admin", "dev-proxy-fixture-password-2026"
	app := browserTestApp(t, true)
	ctx := context.Background()
	if _, err := app.SetupInitialAdmin(ctx, login, "개발 프록시", password); err != nil {
		t.Fatal(err)
	}
	account := &domain.MailAccount{ID: "dev-proxy-account", UserID: application.DefaultUserID, Name: "검증 계정", Email: "dev@corp.local", Status: domain.AccountActive}
	if err := app.Store.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	message := &domain.Message{ID: "dev-proxy-message", UserID: application.DefaultUserID, AccountID: account.ID, UIDL: "proxy-message", RawHash: "proxy-message", Subject: "개발 프록시 메일", From: domain.Address{Email: "sender@corp.local"}, Date: time.Now().Unix()}
	if err := app.Store.InsertMessage(ctx, message, &domain.MessageBody{MessageID: message.ID, TextBody: "인증된 본문", HTMLSanitized: receivedhtml.Sanitize("<p>Vite 인증된 본문</p>")}, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(app, "").Handler())
	t.Cleanup(server.Close)
	commandCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(commandCtx, "node", "../../../web/e2e/dev-proxy.cjs")
	command.Env = append(os.Environ(), "POSTRA_DEV_API_TARGET="+server.URL, "POSTRA_TEST_LOGIN="+login, "POSTRA_TEST_PASSWORD="+password)
	output, err := command.CombinedOutput()
	safe := strings.ReplaceAll(string(output), password, "[REDACTED]")
	if err != nil {
		t.Fatalf("dev proxy browser: %v\n%s", err, safe)
	}
	t.Log(safe)
}
