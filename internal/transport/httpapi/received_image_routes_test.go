package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/receivedhtml"
)

func TestReceivedImagePolicyConsentAndOwnerIsolation(t *testing.T) {
	app := browserTestApp(t, true)
	owner, csrf := browserIdentity(t, app, "images-owner", domain.RoleAdmin)
	other, _ := browserIdentity(t, app, "images-other", domain.RoleUser)
	for _, id := range []string{"images-owner", "images-other"} {
		message := &domain.Message{ID: "msg-" + id, UserID: id, AccountID: "acc-" + id, UIDL: id, RawHash: id, From: domain.Address{Email: "sender@corp.local"}, Subject: "수신 이미지"}
		if err := app.Store.InsertMessage(context.Background(), message, &domain.MessageBody{MessageID: message.ID, TextBody: "본문", HTMLSanitized: receivedhtml.Sanitize(`<p>본문</p><img src="https://images.corp.local/photo.png"><img src="https://images.corp.local/pixel.gif" width="1">`)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	adminCtx := application.WithPrincipal(context.Background(), domain.Principal{UserID: "images-owner", Role: domain.RoleAdmin})
	policy := func(value string) {
		t.Helper()
		if _, err := app.AdminPatchSettings(adminCtx, application.SettingsPatch{Values: map[string]string{"mail.external_images": value}}); err != nil {
			t.Fatal(err)
		}
	}
	h := New(app, "").Handler()
	get := func(session, id, suffix string) *application.MessageView {
		t.Helper()
		response := browserRequest(h, "GET", "/api/messages/"+id+suffix, session, "", "", "")
		if response.Code != 200 {
			t.Fatalf("message: %d %s", response.Code, response.Body.String())
		}
		var view application.MessageView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return &view
	}
	assertImage := func(view *application.MessageView, allowed bool) {
		t.Helper()
		if view.Body == nil || view.Body.ImagesAllowed != allowed || view.Body.ExternalImages != 1 || strings.Contains(view.Body.HTMLSanitized, `src="https://images.corp.local/photo.png"`) != allowed || strings.Contains(view.Body.HTMLSanitized, "pixel.gif") {
			t.Fatalf("unsafe image policy response: %+v", view.Body)
		}
	}
	assertImage(get(owner, "msg-images-owner", "?external_images=once"), false)
	if response := browserRequest(h, "POST", "/api/messages/msg-images-owner/images/allow", owner, csrf, "https://postra.test", `{"scope":"sender"}`); response.Code != 403 {
		t.Fatalf("admin block bypassed: %d", response.Code)
	}
	policy("allow_once")
	assertImage(get(owner, "msg-images-owner", "?external_images=once"), true)
	assertImage(get(owner, "msg-images-owner", ""), false)
	frameRequest := httptest.NewRequest("GET", "https://postra.test/api/messages/msg-images-owner/body/frame?external_images=once", nil)
	frameRequest.AddCookie(&http.Cookie{Name: "postra_session", Value: owner})
	frameRequest.Header.Set("Sec-Fetch-Site", "same-origin")
	frameRequest.Header.Set("Sec-Fetch-Dest", "iframe")
	frame := httptest.NewRecorder()
	h.ServeHTTP(frame, frameRequest)
	if frame.Code != 200 || !strings.HasPrefix(frame.Header().Get("Content-Type"), "text/html") || !strings.Contains(frame.Header().Get("Content-Security-Policy"), "sandbox allow-popups allow-popups-to-escape-sandbox; default-src 'none'; img-src https: http:") || strings.Contains(frame.Header().Get("Content-Security-Policy"), "allow-same-origin") || !strings.Contains(frame.Body.String(), `src="https://images.corp.local/photo.png"`) || frame.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("received frame unsafe: %d %s", frame.Code, frame.Body.String())
	}
	if response := browserRequest(h, "GET", "/api/messages/msg-images-other/body/frame", owner, "", "", ""); response.Code != 404 {
		t.Fatalf("admin frame privacy leak: %d", response.Code)
	}
	if direct := browserRequest(h, "GET", "/api/messages/msg-images-owner/body/frame?external_images=once", owner, "", "", ""); !strings.Contains(direct.Header().Get("Content-Security-Policy"), "img-src 'none'") || strings.Contains(direct.Body.String(), `src="https://images.corp.local/photo.png"`) {
		t.Fatal("top-level untrusted navigation forced one-time image consent")
	}
	policy("allow_domain")
	if response := browserRequest(h, "POST", "/api/messages/msg-images-owner/images/allow", owner, csrf, "https://evil.test", `{"scope":"domain"}`); response.Code != 403 {
		t.Fatalf("CSRF accepted: %d", response.Code)
	}
	if response := browserRequest(h, "POST", "/api/messages/msg-images-other/images/allow", owner, csrf, "https://postra.test", `{"scope":"domain"}`); response.Code != 404 {
		t.Fatalf("admin modified other's trust: %d", response.Code)
	}
	if response := browserRequest(h, "POST", "/api/messages/msg-images-owner/images/allow", owner, csrf, "https://postra.test", `{"scope":"domain"}`); response.Code != 204 {
		t.Fatalf("grant: %d %s", response.Code, response.Body.String())
	}
	h = New(app, "").Handler()
	view := get(owner, "msg-images-owner", "")
	assertImage(view, true)
	if !view.Body.ImageDomainTrusted || view.Body.ImageSenderTrusted {
		t.Fatal("trust scope not preserved")
	}
	assertImage(get(other, "msg-images-other", ""), false)
	policy("block")
	assertImage(get(owner, "msg-images-owner", "?external_images=once"), false)
	policy("allow_domain")
	if response := browserRequest(h, "DELETE", "/api/messages/msg-images-owner/images/allow", owner, csrf, "https://postra.test", `{"scope":"domain"}`); response.Code != 204 {
		t.Fatalf("revoke: %d", response.Code)
	}
	assertImage(get(owner, "msg-images-owner", ""), false)
	if _, err := app.AdminPatchSettings(adminCtx, application.SettingsPatch{Values: map[string]string{"mail.html_enabled": "false"}}); err != nil {
		t.Fatal(err)
	}
	if view := get(owner, "msg-images-owner", ""); view.Body.HTMLSanitized != "" {
		t.Fatal("HTML disable ignored")
	}
}

func TestLegacyReceivedHTMLRecoveryDoesNotMutateStorage(t *testing.T) {
	app := browserTestApp(t, true)
	session, _ := browserIdentity(t, app, "legacy-images", domain.RoleUser)
	raw := "From: sender@corp.local\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>본문</p><img src=\"https://images.corp.local/old.png\" onerror=\"alert(1)\">"
	uri, hash, _, err := app.Objects.Put("raw", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	message := &domain.Message{ID: "legacy-image", UserID: "legacy-images", AccountID: "acc-legacy-images", UIDL: "legacy-image", RawURI: uri, RawHash: hash, From: domain.Address{Email: "sender@corp.local"}}
	if err := app.Store.InsertMessage(context.Background(), message, &domain.MessageBody{MessageID: message.ID, TextBody: "본문", HTMLSanitized: "<p>본문</p>"}, nil); err != nil {
		t.Fatal(err)
	}
	response := browserRequest(New(app, "").Handler(), "GET", "/api/messages/legacy-image", session, "", "", "")
	var view application.MessageView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || view.Body.ExternalImages != 1 || view.Body.ImagesAllowed || strings.Contains(view.Body.HTMLSanitized, "onerror") || strings.Contains(view.Body.HTMLSanitized, `src=`) {
		t.Fatalf("legacy recovery: %s", response.Body.String())
	}
	body, err := app.Store.GetBody(context.Background(), "legacy-images", message.ID)
	if err != nil || body.HTMLSanitized != "<p>본문</p>" {
		t.Fatal("legacy read rewrote stored body")
	}
}

func TestLegacyAPITokenRejectsDisabledDefaultUser(t *testing.T) {
	app := browserTestApp(t, true)
	u, err := app.Store.GetUser(context.Background(), application.DefaultUserID)
	if err != nil {
		t.Fatal(err)
	}
	h := New(app, "test-only-token").Handler()
	request := func() int {
		r := httptest.NewRequest("GET", "https://postra.test/api/me", nil)
		r.Header.Set("Authorization", "Bearer test-only-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(); got != http.StatusOK {
		t.Fatalf("active token: %d", got)
	}
	u.Status = domain.UserDisabled
	if err := app.Store.UpdateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if got := request(); got != http.StatusUnauthorized {
		t.Fatalf("disabled user token accepted: %d", got)
	}
}
