package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
)

func validateToolSuccess(t *testing.T, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var wire any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	resolved, err := outputSchema(name).Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(wire); err != nil {
		t.Fatalf("%s success does not match its published output contract: %v", name, err)
	}
}

func TestMCPOutputSchemasCoverCatalogAndGoWire(t *testing.T) {
	app, ctx, _ := convergenceApp(t)
	_, key, err := app.CreateMCPKey(ctx, "schema discovery")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL, key)
	list, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		canonical := application.CanonicalMCPTool(tool.Name)
		typ := outputTypes()[canonical]
		if typ == nil {
			t.Fatalf("missing typed success output for %s", tool.Name)
		}
		want, err := json.Marshal(outputSchema(canonical))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var left, right any
		_ = json.Unmarshal(want, &left)
		_ = json.Unmarshal(actual, &right)
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("published schema differs for %s", tool.Name)
		}
		if len(outputSchema(canonical).Properties) == 0 {
			t.Fatalf("untyped object for %s", tool.Name)
		}
		validateToolSuccess(t, tool.Name, reflect.Zero(typ).Interface())
	}
	for name, invalid := range map[string]any{
		"mail_send_request_approval": map[string]any{"preview": "wrong", "approval": nil},
		"mail_send":                  map[string]any{"status": true},
		"mail_send_preview":          map[string]any{"draft_id": 9},
		"mail_account_list":          map[string]any{"accounts": "wrong"},
		"mail_render":                map[string]any{"body_html": 2},
		"mail_question_answer":       map[string]any{"retrieval": "wrong"},
	} {
		resolved, err := outputSchema(name).Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Validate(invalid) == nil {
			t.Errorf("%s accepted malformed output", name)
		}
	}
}

type outputContractAI struct {
	mu       sync.Mutex
	requests []domain.GenerationRequest
}

func (a *outputContractAI) Generate(_ context.Context, in domain.GenerationRequest) (domain.GenerationResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, in)
	a.mu.Unlock()
	return domain.GenerationResult{Text: `{"summary":"견적서 검토 요청","answer":"견적서를 검토하고 회신해야 합니다.","evidence_message_ids":["msg_contract_source"]}`, Model: "local-contract-fixture"}, nil
}
func (*outputContractAI) Embed(context.Context, domain.EmbeddingRequest) (domain.EmbeddingResult, error) {
	return domain.EmbeddingResult{}, errors.New("keyword scenario must not request embeddings")
}

func TestMCPSearchSummaryReplyApprovalDeliveryOutputContracts(t *testing.T) {
	app, ctx, smtp := convergenceApp(t)
	ai := &outputContractAI{}
	app.AI = ai
	owner, _ := application.PrincipalFrom(ctx)
	for _, message := range []*domain.Message{
		{ID: "msg_contract_source", UserID: owner.UserID, AccountID: "acc_mcp", UIDL: "contract-owned", Subject: "contractneedle 견적서 검토", From: domain.Address{Email: "colleague@corp.local"}, To: []domain.Address{{Email: "me@corp.local"}}, MessageID: "<contract-source@corp.local>", ThreadID: "contract-thread", RawHash: "contract-owned", Date: time.Now().Unix(), CreatedAt: time.Now().Unix()},
		{ID: "msg_contract_foreign", UserID: "other-user", AccountID: "acc_private", UIDL: "contract-foreign", Subject: "contractneedle private", From: domain.Address{Email: "private@corp.local"}, MessageID: "<contract-private@corp.local>", ThreadID: "contract-thread", RawHash: "contract-foreign", Date: time.Now().Unix(), CreatedAt: time.Now().Unix()},
	} {
		if err := app.Store.InsertMessage(ctx, message, &domain.MessageBody{MessageID: message.ID, TextBody: "untrusted-contract-body: 견적서를 검토하고 회신해주세요."}, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, key, err := app.CreateMCPKeyWithScopes(ctx, "full mail contract", []string{"mail.read", "mail.search", "mail.ai", "mail.draft", "mail.send", "mail.work"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL, key)
	for _, tool := range []string{"mail_capabilities", "mail_identity", "mail_system_info", "mail_account_list", "mail_templates", "mail_signature_list", "mail_drafts_list", "mail_outbound_list", "job_list", "mail_events"} {
		mcpDecode[map[string]any](t, mcpCall(t, client, tool, map[string]any{}))
	}
	search := mcpDecode[domain.SearchResult](t, mcpCall(t, client, "mail_search", map[string]any{"text": "contractneedle"}))
	if len(search.Messages) != 1 || search.Messages[0].ID != "msg_contract_source" {
		t.Fatalf("search crossed owner boundary: %+v", search)
	}
	sourceID := search.Messages[0].ID
	summary := mcpDecode[domain.Analysis](t, mcpCall(t, client, "mail_summarize", map[string]any{"message_id": sourceID}))
	if summary.TargetID != sourceID || !strings.Contains(summary.ResultJSON, "견적서") {
		t.Fatalf("wrong summary context: %+v", summary)
	}
	thread := mcpDecode[application.ThreadView](t, mcpCall(t, client, "mail_thread_get", map[string]any{"thread_id": "contract-thread", "include_bodies": true}))
	if len(thread.Messages) != 1 || thread.Messages[0].Message.ID != sourceID {
		t.Fatal("thread schema flow leaked a foreign message")
	}
	mcpCall(t, client, "mail_message_get", map[string]any{"message_id": sourceID, "include_body": true})
	ask := mcpDecode[application.AskResult](t, mcpCall(t, client, "mail_ask", map[string]any{"question": "어떤 회신이 필요한가요?", "search_text": "contractneedle", "mode": "keyword"}))
	if len(ask.Retrieval.Sources) != 1 || ask.Retrieval.Sources[0].MessageID != sourceID {
		t.Fatal("Ask alias lost scoped source evidence")
	}
	rendered := mcpDecode[map[string]any](t, mcpCall(t, client, "mail_render", map[string]any{"account_id": "acc_mcp", "body": "검토하겠습니다.\n\n감사합니다.", "format": "text"}))
	draft := mcpDecode[application.DraftView](t, mcpCall(t, client, "mail_draft_create", map[string]any{"account_id": "acc_mcp", "kind": "reply", "reply_to_message_id": sourceID, "body_html": rendered["body_html"], "format": "html"}))
	if draft.Draft.ReplyToMessageID != sourceID || len(draft.Version.To) != 1 || draft.Version.To[0].Email != "colleague@corp.local" {
		t.Fatalf("reply lost original source: %+v", draft)
	}
	mcpCall(t, client, "mail_draft_get", map[string]any{"draft_id": draft.Draft.ID})
	preview := mcpDecode[application.SendPreview](t, mcpCall(t, client, "mail_send_preview", map[string]any{"draft_id": draft.Draft.ID}))
	if preview.DraftVersion != draft.Version.Version || preview.Body == "" {
		t.Fatal("preview is not the exact saved draft")
	}
	smtp.mu.Lock()
	count := len(smtp.messages)
	smtp.mu.Unlock()
	if count != 0 {
		t.Fatal("read/summary/draft/preview sent mail without approval")
	}
	approved := mcpDecode[sendApprovalOutput](t, mcpCall(t, client, "mail_send_request_approval", map[string]any{"draft_id": draft.Draft.ID, "approver": "contract-user"}))
	if approved.Preview.PayloadHash != preview.PayloadHash || approved.Approval.Token == "" {
		t.Fatal("approval envelope lost immutable payload")
	}
	sent := mcpDecode[domain.OutboundMessage](t, mcpCall(t, client, "mail_send", map[string]any{"draft_id": draft.Draft.ID, "approval_token": approved.Approval.Token, "idempotency_key": "contract-flow-once"}))
	status := mcpDecode[domain.OutboundMessage](t, mcpCall(t, client, "mail_outbound_status", map[string]any{"outbound_id": sent.ID}))
	if status.Status != domain.OutboundSent || status.DraftVersion != preview.DraftVersion {
		t.Fatalf("delivery status diverged: %+v", status)
	}
	mcpCall(t, client, "mail_outbound_list", map[string]any{})
	smtp.mu.Lock()
	defer smtp.mu.Unlock()
	if len(smtp.messages) != 1 || !strings.Contains(smtp.messages[0], "In-Reply-To: <contract-source@corp.local>") {
		t.Fatal("reply did not produce one SMTP message with original threading context")
	}
	ai.mu.Lock()
	defer ai.mu.Unlock()
	if len(ai.requests) < 2 {
		t.Fatal("summary and Ask did not call the configured fake AI")
	}
	for _, request := range ai.requests {
		if !strings.Contains(request.Untrusted, "untrusted-contract-body") || strings.Contains(request.User, "untrusted-contract-body") || strings.Contains(request.Untrusted, "msg_contract_foreign") {
			t.Fatal("AI evidence was not isolated as owned untrusted data")
		}
	}
}
