package webui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/handoff"
)

func seedHandoffMessage(t *testing.T, app *application.App) {
	t.Helper()
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_h", UserID: application.DefaultUserID, AccountID: "acc_1", UIDL: "uh",
		Subject: "handoff", From: domain.Address{Email: "a@example.com"}, RawHash: "hh", RawURI: "mem://h",
		Date: now, CreatedAt: now,
	}
	if err := app.Store.InsertMessage(context.Background(), m, &domain.MessageBody{MessageID: m.ID, TextBody: "b"}, nil); err != nil {
		t.Fatal(err)
	}
}

// The "send to" buttons exist only for allow-listed services that read
// markdown; a fresh installation shows none.
func TestHandoffButtonsFollowTheAllowList(t *testing.T) {
	app, _ := newTestApp(t)
	seedHandoffMessage(t, app)
	app.Cfg.Auth.Enabled = true
	if _, err := app.SetupInitialAdmin(context.Background(), "admin", "Administrator", "a-secure-password"); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()
	login := do(t, h, http.MethodPost, "/ui/login", url.Values{"login_id": {"admin"}, "password": {"a-secure-password"}}, nil)
	var admin *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == cookieName {
			admin = c
		}
	}
	if admin == nil {
		t.Fatalf("login: %d", login.Code)
	}

	rec := do(t, h, http.MethodGet, "/ui/messages/msg_h", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("message page: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `data-handoff-origin="`) || strings.Contains(rec.Body.String(), "다른 서비스로 보내기") {
		t.Fatal("no targets configured: the message page must not offer a send button")
	}

	// The administrator names two services; only the markdown reader appears.
	form := url.Values{
		handoff.SettingTargets: {`[{"name":"Ptium","origin":"https://ptium.intra","formats":["markdown","docx"]},
		                          {"name":"Kanpic","origin":"https://kanpic.intra","formats":["csv","xlsx"]}]`},
		handoff.SettingSourceOrigin: {"https://postra.intra"},
	}
	saved := do(t, h, http.MethodPost, "/ui/admin/settings", form, admin)
	if saved.Code != http.StatusSeeOther {
		t.Fatalf("saving the allow list: %d %s", saved.Code, saved.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/ui/messages/msg_h", nil, admin)
	body := rec.Body.String()
	if !strings.Contains(body, `data-handoff-origin="https://ptium.intra" data-handoff-resource="msg_h"`) || !strings.Contains(body, "Ptium 으로 보내기") {
		t.Fatalf("Ptium reads markdown and must get a button:\n%s", body)
	}
	if strings.Contains(body, "kanpic.intra") {
		t.Fatal("Kanpic does not read markdown; no button may be made for it")
	}
	// The layout's handler that turns the button into a claim runs under the nonce.
	if !strings.Contains(body, "fetch('/api/v1/handoff/claims'") {
		t.Fatal("the page must carry the handoff script")
	}

	// The settings screen shows the list back and refuses a broken one.
	settings := do(t, h, http.MethodGet, "/ui/admin/settings", nil, admin)
	if !strings.Contains(settings.Body.String(), `name="handoff.targets"`) || !strings.Contains(settings.Body.String(), "ptium.intra") {
		t.Fatal("settings page must show the allow list")
	}
	bad := do(t, h, http.MethodPost, "/ui/admin/settings", url.Values{handoff.SettingTargets: {`[{"name":"X","origin":"https://x.intra/path","formats":["markdown"]}]`}}, admin)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "다른 서비스로 보내기") {
		t.Fatalf("a target with a path must be refused with the reason shown: %d", bad.Code)
	}
}
