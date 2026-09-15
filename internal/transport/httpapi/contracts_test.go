package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"postra/internal/domain"
	"postra/internal/transport/httpapi/contracts"
)

func contractResponse(t *testing.T, schema string, response *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if response.Code != status {
		t.Fatalf("%s status %d: %s", schema, response.Code, response.Body.String())
	}
	var data map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if err := contracts.Validate(schema, data); err != nil {
		t.Fatalf("%s wire drift: %v\n%s", schema, err, response.Body.String())
	}
	return data
}

func TestCanonicalCoreContractResponses(t *testing.T) {
	app := browserTestApp(t, true)
	app.AI = spaBrowserAI{}
	owner, csrf := browserIdentity(t, app, "contract-owner", domain.RoleUser)
	admin, adminCSRF := browserIdentity(t, app, "contract-admin", domain.RoleAdmin)
	ctx := context.Background()
	account, err := app.Store.GetAccount(ctx, "contract-owner", "acc-contract-owner")
	if err != nil {
		t.Fatal(err)
	}
	account.SMTPHost, account.SMTPPort, account.SMTPSecurity, account.SMTPAuth = "smtp.corp.local", 465, domain.SecurityTLS, "none"
	if err := app.Store.UpdateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	message := &domain.Message{ID: "msg-contract", UserID: "contract-owner", AccountID: account.ID, UIDL: "contract-fixture", RawHash: "contract-fixture", Subject: "계약 검토", From: domain.Address{Email: "sender@corp.local"}, Date: time.Now().Unix()}
	if err := app.Store.InsertMessage(ctx, message, &domain.MessageBody{MessageID: message.ID, TextBody: "계약 검토가 필요합니다."}, nil); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()
	for _, prefix := range []string{"/api/v1", "/api"} {
		got := browserRequest(h, "GET", prefix+"/preferences", owner, "", "", "")
		contractResponse(t, "SettingsView", got, 200)
		if (got.Header().Get("Deprecation") == "true") != (prefix == "/api") {
			t.Fatalf("canonical/legacy deprecation mismatch: %s", prefix)
		}
	}
	contractResponse(t, "SettingsView", browserRequest(h, "GET", "/api/v1/admin/configuration", admin, "", "", ""), 200)
	contractResponse(t, "SettingsView", browserRequest(h, "PATCH", "/api/v1/preferences", owner, csrf, "https://postra.test", `{"values":{"compose.template":"formal"}}`), 200)
	contractResponse(t, "SettingsView", browserRequest(h, "PATCH", "/api/v1/accounts/acc-contract-owner/preferences", owner, csrf, "https://postra.test", `{"values":{"compose.tone":"professional"}}`), 200)
	contractResponse(t, "RenderedMail", browserRequest(h, "POST", "/api/v1/mail/render", owner, csrf, "https://postra.test", `{"account_id":"acc-contract-owner","body":"# 계약 검토","format":"auto"}`), 200)
	signature := contractResponse(t, "MailSignature", browserRequest(h, "POST", "/api/v1/signatures", owner, csrf, "https://postra.test", `{"name":"기본","display_name":"홍길동"}`), 201)
	contractResponse(t, "MailSignature", browserRequest(h, "GET", "/api/v1/signatures/"+signature["id"].(string), owner, "", "", ""), 200)
	draft := contractResponse(t, "DraftView", browserRequest(h, "POST", "/api/v1/drafts", owner, csrf, "https://postra.test", `{"account_id":"acc-contract-owner","to":["reader@corp.local"],"bcc":["private@corp.local"],"subject":"검토 요청","body":"# 계약\n\n검토해 주세요.","format":"auto"}`), 201)
	draftID := draft["draft"].(map[string]any)["id"].(string)
	path := "/api/v1/drafts/" + draftID
	contractResponse(t, "DraftView", browserRequest(h, "PATCH", path, owner, csrf, "https://postra.test", `{"subject":"계약 검토 요청"}`), 200)
	upload := contractResponse(t, "DraftView", browserRequest(h, "POST", path+"/attachments", owner, csrf, "https://postra.test", `{"name":"report.txt","data_base64":"dGVzdA=="}`), 201)
	attachments := upload["version"].(map[string]any)["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatal("attachment missing")
	}
	if err := contracts.Validate("DraftAttachment", attachments[0]); err != nil {
		t.Fatal(err)
	}
	contractResponse(t, "SendPreview", browserRequest(h, "POST", path+"/preview", owner, csrf, "https://postra.test", `{}`), 200)
	contractResponse(t, "AskResult", browserRequest(h, "POST", "/api/v1/qa", owner, csrf, "https://postra.test", `{"question":"계약 관련 요청을 알려줘","mode":"keyword","search_text":"계약","time_zone":"Asia/Seoul"}`), 200)
	contractResponse(t, "ErrorResponse", browserRequest(h, "GET", path, admin, adminCSRF, "https://postra.test", ""), 404)
}
