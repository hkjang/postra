package httpapi

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"postra/internal/application"
	"postra/internal/domain"
)

func TestRenderingRoutesShareOwnershipAndCSRF(t *testing.T) {
	app := browserTestApp(t, true)
	owner, ownerCSRF := browserIdentity(t, app, "render-owner", domain.RoleUser)
	admin, adminCSRF := browserIdentity(t, app, "render-admin", domain.RoleAdmin)
	h := New(app, "").Handler()
	request := `{"name":"개인 서명","display_name":"홍길동","company":"Postra","email":"hong@corp.local"}`
	if got := browserRequest(h, "POST", "/api/signatures", owner, "", "https://postra.test", request); got.Code != 403 {
		t.Fatalf("signature save bypassed CSRF: %d", got.Code)
	}
	created := browserRequest(h, "POST", "/api/signatures", owner, ownerCSRF, "https://postra.test", request)
	var signature domain.MailSignature
	if err := json.Unmarshal(created.Body.Bytes(), &signature); created.Code != 201 || err != nil || signature.UserID != "render-owner" || !strings.Contains(signature.BodyText, "홍길동") {
		t.Fatalf("create: %d %s %v", created.Code, created.Body.String(), err)
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		got := browserRequest(h, method, "/api/signatures/"+signature.ID, admin, adminCSRF, "https://postra.test", `{"name":"overwrite","body":"private"}`)
		if got.Code != 404 {
			t.Fatalf("administrator accessed another user's signature: %s %d %s", method, got.Code, got.Body.String())
		}
	}
	list := browserRequest(h, "GET", "/api/signatures", admin, "", "", "")
	if list.Code != 200 || strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("private signature listing: %d %s", list.Code, list.Body.String())
	}
	rendered := browserRequest(h, "POST", "/api/mail/render", owner, ownerCSRF, "https://postra.test", `{"account_id":"acc-render-owner","body":"# 안녕하세요","format":"auto","template":"formal","signature_id":"`+signature.ID+`"}`)
	if rendered.Code != 200 || !strings.Contains(rendered.Body.String(), `"template":"formal"`) || !strings.Contains(rendered.Body.String(), "홍길동") {
		t.Fatalf("render route: %d %s", rendered.Code, rendered.Body.String())
	}
	stolen := browserRequest(h, "POST", "/api/mail/render", admin, adminCSRF, "https://postra.test", `{"account_id":"acc-render-owner","body":"body","signature_id":"`+signature.ID+`"}`)
	if stolen.Code != 404 {
		t.Fatalf("render crossed account ownership: %d %s", stolen.Code, stolen.Body.String())
	}
	templates := browserRequest(h, "GET", "/api/mail/templates", owner, "", "", "")
	if templates.Code != 200 || !strings.Contains(templates.Body.String(), `"id":"newsletter"`) {
		t.Fatalf("templates: %d %s", templates.Code, templates.Body.String())
	}
}

func TestDraftAttachmentRoutesUploadVersionPrivacyAndSafeDownload(t *testing.T) {
	app := browserTestApp(t, true)
	owner, csrf := browserIdentity(t, app, "attachment-owner", domain.RoleUser)
	admin, adminCSRF := browserIdentity(t, app, "attachment-admin", domain.RoleAdmin)
	h := New(app, "").Handler()
	created := browserRequest(h, "POST", "/api/drafts", owner, csrf, "https://postra.test", `{"account_id":"acc-attachment-owner","subject":"첨부 테스트","body":"본문","format":"auto"}`)
	var draft application.DraftView
	if err := json.Unmarshal(created.Body.Bytes(), &draft); err != nil || created.Code != 201 {
		t.Fatalf("draft: %d %s %v", created.Code, created.Body.String(), err)
	}
	path := "/api/drafts/" + draft.Draft.ID + "/attachments"
	input := `{"name":"진행 보고.txt","data_base64":"dGVzdA=="}`
	if got := browserRequest(h, "POST", path, owner, "", "https://postra.test", input); got.Code != 403 {
		t.Fatalf("upload bypassed CSRF: %d", got.Code)
	}
	if got := browserRequest(h, "POST", path, admin, adminCSRF, "https://postra.test", input); got.Code != 404 {
		t.Fatalf("admin uploaded into another draft: %d %s", got.Code, got.Body.String())
	}
	upload := browserRequest(h, "POST", path, owner, csrf, "https://postra.test", input)
	if err := json.Unmarshal(upload.Body.Bytes(), &draft); err != nil || upload.Code != 201 || len(draft.Version.Attachments) != 1 || draft.Version.Version != 2 {
		t.Fatalf("upload: %d %s %v", upload.Code, upload.Body.String(), err)
	}
	attachment := draft.Version.Attachments[0]
	downloadPath := path + "/" + attachment.ID + "?version=" + strconv.Itoa(draft.Version.Version)
	down := browserRequest(h, "GET", downloadPath, owner, "", "", "")
	_, params, err := mime.ParseMediaType(down.Header().Get("Content-Disposition"))
	if down.Code != 200 || down.Body.String() != "test" || err != nil || params["filename"] != "진행 보고.txt" || down.Header().Get("Cache-Control") != "no-store" || down.Header().Get("X-Content-Type-Options") != "nosniff" || down.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("download safety: %d %v %s", down.Code, down.Header(), down.Body.String())
	}
	for _, method := range []string{"GET", "DELETE"} {
		got := browserRequest(h, method, path+"/"+attachment.ID, admin, adminCSRF, "https://postra.test", "")
		if got.Code != 404 {
			t.Fatalf("admin accessed other attachment: %s %d", method, got.Code)
		}
	}
	removed := browserRequest(h, "DELETE", path+"/"+attachment.ID, owner, csrf, "https://postra.test", "")
	draft = application.DraftView{}
	if err := json.Unmarshal(removed.Body.Bytes(), &draft); err != nil || removed.Code != 200 || len(draft.Version.Attachments) != 0 || draft.Version.Version != 3 {
		t.Fatalf("remove: %d %s %v", removed.Code, removed.Body.String(), err)
	}
	if old := browserRequest(h, "GET", downloadPath, owner, "", "", ""); old.Code != 200 || old.Body.String() != "test" {
		t.Fatal("new-version removal destroyed historical attachment")
	}
	var body bytes.Buffer
	multipartBody := multipart.NewWriter(&body)
	file, err := multipartBody.CreateFormFile("file", "upload.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("multipart body"))
	if err := multipartBody.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "https://postra.test"+path, &body)
	req.Header.Set("Content-Type", multipartBody.FormDataContentType())
	req.Header.Set("Origin", "https://postra.test")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(&http.Cookie{Name: "postra_session", Value: owner})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("multipart route: %d %s", w.Code, w.Body.String())
	}
	invalid := browserRequest(h, "POST", path, owner, csrf, "https://postra.test", "{secret-malformed-content")
	var public domain.ErrorResponse
	if err := json.Unmarshal(invalid.Body.Bytes(), &public); invalid.Code != 400 || err != nil || public.Code != "invalid_request" || public.TraceID == "" || strings.Contains(invalid.Body.String(), "secret-malformed-content") {
		t.Fatalf("public upload error: %d %s", invalid.Code, invalid.Body.String())
	}
}
