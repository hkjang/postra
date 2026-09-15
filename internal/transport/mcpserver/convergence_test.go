package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/adapters/secretstore"
	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
)

type convergenceSMTP struct {
	mu       sync.Mutex
	messages []string
}

func (s *convergenceSMTP) TestConnection(context.Context, domain.SMTPSendOptions) (*domain.ConnDiagnostics, error) {
	return &domain.ConnDiagnostics{OK: true}, nil
}
func (s *convergenceSMTP) Send(_ context.Context, options domain.SMTPSendOptions, _ domain.Envelope, body io.Reader) (domain.SendReceipt, error) {
	if options.AuthMethod != "none" {
		return domain.SendReceipt{}, errors.New("test requires unauthenticated SMTP")
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return domain.SendReceipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, string(raw))
	return domain.SendReceipt{ServerResponse: "250 local test fake"}, nil
}

func convergenceApp(t *testing.T) (*application.App, context.Context, *convergenceSMTP) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir, cfg.Auth.Enabled, cfg.Auth.BootstrapAdmin, cfg.Auth.BootstrapPassword = dir, true, "mcp-admin", "mcp-fixture-password-2026"
	kek, err := crypto.LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := persistence.Open(filepath.Join(dir, "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	local, err := objectstore.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	smtp := &convergenceSMTP{}
	app, err := application.New(cfg, store, objectstore.NewEncrypted(local, kek), secretstore.NewLocal(dir, kek), nil, smtp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)
	user, err := store.GetUser(context.Background(), application.DefaultUserID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := application.WithPrincipal(context.Background(), domain.Principal{UserID: user.ID, LoginID: user.LoginID, DisplayName: user.DisplayName, Role: domain.RoleAdmin, AuthMethod: "local"})
	for _, account := range []*domain.MailAccount{
		{ID: "acc_mcp", UserID: user.ID, Name: "내 계정", Email: "me@corp.local", Status: domain.AccountActive, SMTPAuth: "none", SMTPHost: "127.0.0.1", SMTPPort: 25, SMTPSecurity: domain.SecurityNone},
		{ID: "acc_private", UserID: "other-user", Name: "비공개", Email: "other@corp.local", Status: domain.AccountActive},
	} {
		if err := store.CreateAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	return app, ctx, smtp
}

type bearerRoundTripper struct {
	key       string
	mu        sync.Mutex
	sessionID string
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header.Set("Authorization", "Bearer "+b.key)
	response, err := http.DefaultTransport.RoundTrip(copy)
	if err == nil && response.Header.Get("Mcp-Session-Id") != "" {
		b.mu.Lock()
		b.sessionID = response.Header.Get("Mcp-Session-Id")
		b.mu.Unlock()
	}
	return response, err
}

func convergenceClient(t *testing.T, serverURL, key string) (*mcp.ClientSession, *bearerRoundTripper) {
	t.Helper()
	transport := &bearerRoundTripper{key: key}
	client := mcp.NewClient(&mcp.Implementation{Name: "postra-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: serverURL, HTTPClient: &http.Client{Transport: transport}}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(); cancel() })
	return session, transport
}

func mcpCall(t *testing.T, session *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s protocol failure: %v", name, err)
	}
	if !result.IsError {
		validateToolSuccess(t, name, result.StructuredContent)
	}
	return result
}

func mcpDecode[T any](t *testing.T, result *mcp.CallToolResult) T {
	t.Helper()
	if result.IsError {
		raw, _ := json.Marshal(result.StructuredContent)
		t.Fatalf("unexpected tool error: %s", raw)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func requireToolError(t *testing.T, result *mcp.CallToolResult, code string) domain.ErrorResponse {
	t.Helper()
	if !result.IsError {
		t.Fatal("expected denied tool call")
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var value domain.ErrorResponse
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value.Code != code || value.Message == "" || value.TraceID == "" {
		t.Fatalf("error contract: %+v, wanted %s", value, code)
	}
	return value
}

func TestMCPDiscoveryScopeIsolationAndSessionBinding(t *testing.T) {
	app, ctx, _ := convergenceApp(t)
	_, readKey, err := app.CreateMCPKey(ctx, "reader")
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := app.CreateMCPKeyWithScopes(ctx, "writer", []string{"mail.read", "mail.draft"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	reader, transport := convergenceClient(t, server.URL, readKey)
	list, err := reader.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range list.Tools {
		seen[tool.Name] = true
		if tool.InputSchema == nil || tool.OutputSchema == nil || tool.Annotations == nil || tool.Meta["postra"] == nil {
			t.Errorf("tool lacks discovery contract: %s", tool.Name)
		}
	}
	for _, name := range []string{"mail_capabilities", "mail_identity", "mail_systeminfo", "mail_ask", "mail_render", "mail_draft", "mail_preview", "mail_approve"} {
		if !seen[name] {
			t.Errorf("missing tool %s", name)
		}
	}
	mcpDecode[map[string]any](t, mcpCall(t, reader, "mail_capabilities", map[string]any{}))
	requireToolError(t, mcpCall(t, reader, "mail_draft", map[string]any{"account_id": "acc_mcp", "kind": "new"}), "insufficient_scope")
	requireToolError(t, mcpCall(t, reader, "mail_account_get", map[string]any{"account_id": "acc_private"}), "not_found")
	requireToolError(t, mcpCall(t, reader, "mail_account_get", map[string]any{}), "invalid_request")
	if _, err := reader.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "postra://draft/does-not-exist"}); err == nil || !strings.Contains(err.Error(), "insufficient_scope") {
		t.Fatalf("resource scope bypass: %v", err)
	}
	transport.mu.Lock()
	sessionID := transport.sessionID
	transport.mu.Unlock()
	if sessionID == "" {
		t.Fatal("SDK did not issue a session")
	}
	req, _ := http.NewRequest("GET", server.URL, nil)
	req.Header.Set("Authorization", "Bearer "+otherKey)
	req.Header.Set("Mcp-Session-Id", sessionID)
	req.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("a different key resumed the session: %d", response.StatusCode)
	}
	req, _ = http.NewRequest("POST", server.URL, strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.invalid")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Authorization", "Bearer "+readKey)
	response2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response2.Body.Close()
	if response2.StatusCode != 403 {
		t.Fatalf("cross-origin request accepted: %d", response2.StatusCode)
	}
}

func TestMCPDraftPreviewApprovalSendAndConcurrency(t *testing.T) {
	app, ctx, smtp := convergenceApp(t)
	key, rawKey, err := app.CreateMCPKeyWithScopes(ctx, "mail workflow", []string{"mail.read", "mail.search", "mail.draft", "mail.send", "mail.work"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL, rawKey)
	draft := mcpDecode[application.DraftView](t, mcpCall(t, client, "mail_draft_create", map[string]any{"account_id": "acc_mcp", "kind": "new", "to": []string{"colleague@corp.local"}, "subject": "공유 안내", "body_html": `<p>안녕하세요 <strong>검토</strong> 부탁드립니다.</p><script>steal()</script>`, "format": "html"}))
	preview := mcpDecode[application.SendPreview](t, mcpCall(t, client, "mail_send_preview", map[string]any{"draft_id": draft.Draft.ID}))
	if preview.BodyHTML == "" || strings.Contains(preview.BodyHTML, "script") || preview.Body == "" {
		t.Fatalf("unsafe or missing rendered alternatives: %+v", preview)
	}
	if !mcpCall(t, client, "mail_send", map[string]any{"draft_id": draft.Draft.ID, "approval_token": "not-issued"}).IsError {
		t.Fatal("send without approval succeeded")
	}
	approval := mcpDecode[struct {
		Approval domain.ApprovalToken `json:"approval"`
	}](t, mcpCall(t, client, "mail_send_request_approval", map[string]any{"draft_id": draft.Draft.ID}))
	mcpDecode[application.DraftView](t, mcpCall(t, client, "mail_draft_update", map[string]any{"draft_id": draft.Draft.ID, "subject": "수정된 공유 안내"}))
	if !mcpCall(t, client, "mail_send", map[string]any{"draft_id": draft.Draft.ID, "approval_token": approval.Approval.Token}).IsError {
		t.Fatal("stale approval survived a draft edit")
	}
	approval = mcpDecode[struct {
		Approval domain.ApprovalToken `json:"approval"`
	}](t, mcpCall(t, client, "mail_approve", map[string]any{"draft_id": draft.Draft.ID}))
	input := map[string]any{"draft_id": draft.Draft.ID, "approval_token": approval.Approval.Token, "idempotency_key": "mcp-delivery-once"}
	var wait sync.WaitGroup
	results := make(chan *mcp.CallToolResult, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "mail_send", Arguments: input})
			if err != nil {
				t.Errorf("send RPC: %v", err)
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	succeeded := 0
	for result := range results {
		if !result.IsError {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatal("no concurrent send completed")
	}
	mcpDecode[domain.OutboundMessage](t, mcpCall(t, client, "mail_send", input))
	smtp.mu.Lock()
	messages := append([]string{}, smtp.messages...)
	smtp.mu.Unlock()
	if len(messages) != 1 || !strings.Contains(messages[0], "multipart/alternative") || strings.Contains(messages[0], "<script") {
		t.Fatalf("SMTP delivery count/safety mismatch: %d", len(messages))
	}
	if _, err := app.UpdateMCPKeyScopes(ctx, key.ID, []string{"mail.read"}, false); err != nil {
		t.Fatal(err)
	}
	requireToolError(t, mcpCall(t, client, "mail_send", input), "insufficient_scope")
	if err := app.RevokeMyMCPKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "mail_identity", Arguments: map[string]any{}}); err == nil {
		t.Fatal("revoked key kept access to an existing session")
	}
	audits, err := app.Store.SearchAudit(ctx, application.DefaultUserID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range audits {
		if strings.Contains(event.Detail, rawKey) || strings.Contains(event.Detail, approval.Approval.Token) || strings.Contains(event.Detail, "부탁드립니다") {
			t.Fatal("audit leaked a secret or mail content")
		}
		if event.Action == "mcp_call" && strings.Contains(event.Detail, "input_bytes") && strings.Contains(event.Detail, "trace_id") && strings.Contains(event.Detail, key.ID) {
			found = true
		}
	}
	if !found {
		t.Fatal("missing per-client trace/size audit")
	}
}

func TestMCPDraftAttachmentsShareVersioningAndBoundContent(t *testing.T) {
	app, ctx, _ := convergenceApp(t)
	_, key, err := app.CreateMCPKeyWithScopes(ctx, "attachments", []string{"mail.draft"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL, key)
	draft := mcpDecode[application.DraftView](t, mcpCall(t, client, "mail_draft_create", map[string]any{"account_id": "acc_mcp", "kind": "new", "to": []string{"colleague@corp.local"}, "subject": "첨부 검증", "body": "검토 바랍니다."}))
	const content = "private-owned-attachment-content"
	updated := mcpDecode[application.DraftView](t, mcpCall(t, client, "mail_draft_attachment_add", map[string]any{"draft_id": draft.Draft.ID, "name": "notes.txt", "data_base64": base64.StdEncoding.EncodeToString([]byte(content))}))
	if len(updated.Version.Attachments) != 1 || updated.Version.Version <= draft.Version.Version {
		t.Fatalf("attachment did not version draft: %+v", updated)
	}
	attachmentID := updated.Version.Attachments[0].ID
	metadata := mcpDecode[map[string]any](t, mcpCall(t, client, "mail_draft_attachment_get", map[string]any{"draft_id": draft.Draft.ID, "attachment_id": attachmentID}))
	if metadata["data_base64"] != nil || metadata["storage_uri"] != nil || metadata["download_url"] == nil {
		t.Fatalf("unexpected attachment metadata: %+v", metadata)
	}
	file := mcpDecode[map[string]any](t, mcpCall(t, client, "mail_draft_attachment_get", map[string]any{"draft_id": draft.Draft.ID, "attachment_id": attachmentID, "include_content": true}))
	if file["data_base64"] != base64.StdEncoding.EncodeToString([]byte(content)) {
		t.Fatal("explicit file read returned incorrect data")
	}
	requireToolError(t, mcpCall(t, client, "mail_draft_attachment_add", map[string]any{"draft_id": draft.Draft.ID, "name": "large.txt", "data_base64": strings.Repeat("a", base64.StdEncoding.EncodedLen(mcpAttachmentBytes)+1)}), "payload_too_large")
	requireToolError(t, mcpCall(t, client, "mail_draft_attachment_remove", map[string]any{"draft_id": draft.Draft.ID, "attachment_id": attachmentID, "confirm": false}), "invalid_request")
	removed := mcpDecode[application.DraftView](t, mcpCall(t, client, "mail_draft_attachment_remove", map[string]any{"draft_id": draft.Draft.ID, "attachment_id": attachmentID, "confirm": true}))
	if len(removed.Version.Attachments) != 0 || removed.Version.Version <= updated.Version.Version {
		t.Fatal("confirmed removal did not version draft")
	}
}

func TestMCPErrorSanitization(t *testing.T) {
	app, ctx, _ := convergenceApp(t)
	server := NewServer(app)
	addTool(server, &mcp.Tool{Name: "mail_account_get", Description: "failure fixture", Annotations: readOnly}, func(context.Context, *mcp.CallToolRequest, emptyInput) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("provider key=do-not-disclose-secret")
	})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "error-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	result := mcpCall(t, cs, "mail_account_get", map[string]any{})
	requireToolError(t, result, "internal_error")
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "do-not-disclose") {
		t.Fatal("provider secret reached public MCP error")
	}
}
