package mcpserver

import (
	"net/http/httptest"
	"strings"
	"testing"

	"postra/internal/application"
	"postra/internal/domain"
)

func TestMCPOAuthSendPreservesApprovalPayloadBindingAndIdempotency(t *testing.T) {
	f := newMCPOAuthFixture(t)
	server := httptest.NewServer(HTTPHandler(f.app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL+"/mcp", f.token(t, map[string]any{"scope": "mail.read mail.draft mail.send"}))
	draft := mcpDecode[application.DraftView](t, mcpCall(t, client, "mail_draft_create", map[string]any{"account_id": "acc_mcp", "kind": "new", "to": []string{"colleague@corp.local"}, "subject": "OAuth 업무 안내", "body": "## 요청사항\n\n- 자료 검토\n- 일정 확인", "format": "markdown"}))
	preview := mcpDecode[application.SendPreview](t, mcpCall(t, client, "mail_send_preview", map[string]any{"draft_id": draft.Draft.ID}))
	if preview.Body == "" || preview.BodyHTML == "" {
		t.Fatal("OAuth lost multipart preview")
	}
	if !mcpCall(t, client, "mail_send", map[string]any{"draft_id": draft.Draft.ID, "approval_token": "not-approved"}).IsError {
		t.Fatal("OAuth bypassed approval")
	}
	approval := mcpDecode[sendApprovalOutput](t, mcpCall(t, client, "mail_send_request_approval", map[string]any{"draft_id": draft.Draft.ID}))
	mcpDecode[application.DraftView](t, mcpCall(t, client, "mail_draft_update", map[string]any{"draft_id": draft.Draft.ID, "subject": "수정된 업무 안내"}))
	if !mcpCall(t, client, "mail_send", map[string]any{"draft_id": draft.Draft.ID, "approval_token": approval.Approval.Token}).IsError {
		t.Fatal("OAuth reused stale payload approval")
	}
	approval = mcpDecode[sendApprovalOutput](t, mcpCall(t, client, "mail_send_request_approval", map[string]any{"draft_id": draft.Draft.ID}))
	input := map[string]any{"draft_id": draft.Draft.ID, "approval_token": approval.Approval.Token, "idempotency_key": "oauth-delivery-once"}
	first := mcpDecode[domain.OutboundMessage](t, mcpCall(t, client, "mail_send", input))
	second := mcpDecode[domain.OutboundMessage](t, mcpCall(t, client, "mail_send", input))
	if first.ID == "" || first.ID != second.ID {
		t.Fatal("OAuth retry lost idempotency")
	}
	smtp := f.app.SMTP.(*convergenceSMTP)
	smtp.mu.Lock()
	defer smtp.mu.Unlock()
	if len(smtp.messages) != 1 || !strings.Contains(smtp.messages[0], "multipart/alternative") {
		t.Fatal("OAuth sent duplicate or non-alternative mail")
	}
}
