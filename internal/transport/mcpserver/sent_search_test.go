package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
)

// mail_sent_search returns the caller's sent mail and nothing else — no
// received mail, and nobody else's sent mail — while mail_search's default
// view is unchanged.
func TestMCPSentSearchReturnsOnlyTheCallersSentMail(t *testing.T) {
	app, ctx, _ := convergenceApp(t)
	owner, _ := application.PrincipalFrom(ctx)
	now := time.Now().Unix()
	for _, m := range []*domain.Message{
		{ID: "msg_sent_1", UserID: owner.UserID, AccountID: "acc_mcp", UIDL: "sent:1", Subject: "견적서 회신", From: domain.Address{Email: "me@corp.local"}, To: []domain.Address{{Email: "buyer@partner.test"}}, MessageID: "<s1@corp.local>", RawHash: "sent-1", Date: now - 60, Mailbox: domain.MailboxSent},
		{ID: "msg_sent_2", UserID: owner.UserID, AccountID: "acc_mcp", UIDL: "sent:2", Subject: "회의록 공유", From: domain.Address{Email: "me@corp.local"}, To: []domain.Address{{Email: "team@corp.local"}}, MessageID: "<s2@corp.local>", RawHash: "sent-2", Date: now - 30, Mailbox: domain.MailboxSent},
		{ID: "msg_recv_1", UserID: owner.UserID, AccountID: "acc_mcp", UIDL: "1.1", Subject: "견적서 요청", From: domain.Address{Email: "buyer@partner.test"}, To: []domain.Address{{Email: "me@corp.local"}}, MessageID: "<r1@partner.test>", RawHash: "recv-1", Date: now - 90},
		{ID: "msg_sent_other", UserID: "other-user", AccountID: "acc_private", UIDL: "sent:9", Subject: "견적서 타인", From: domain.Address{Email: "private@corp.local"}, MessageID: "<s9@corp.local>", RawHash: "sent-9", Date: now, Mailbox: domain.MailboxSent},
	} {
		body := &domain.MessageBody{MessageID: m.ID, TextBody: "untrusted body: 견적서 " + m.Subject}
		if err := app.Store.InsertMessage(ctx, m, body, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, key, err := app.CreateMCPKeyWithScopes(ctx, "sent search", []string{"mail.read", "mail.search"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL, key)

	ids := func(tool string, args map[string]any) []string {
		t.Helper()
		res := mcpDecode[domain.SearchResult](t, mcpCall(t, client, tool, args)) // mcpCall also validates the output schema
		var out []string
		for _, m := range res.Messages {
			out = append(out, m.ID)
		}
		return out
	}
	equal := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	if got := ids("mail_sent_search", map[string]any{}); !equal(got, "msg_sent_2", "msg_sent_1") {
		t.Fatalf("all sent mail, newest first = %v", got)
	}
	if got := ids("mail_sent_search", map[string]any{"to": "partner.test"}); !equal(got, "msg_sent_1") {
		t.Fatalf("by recipient = %v", got)
	}
	if got := ids("mail_sent_search", map[string]any{"subject": "견적서"}); !equal(got, "msg_sent_1") {
		t.Fatalf("by subject: a received match or another user's sent mail leaked = %v", got)
	}
	if got := ids("mail_search", map[string]any{}); !equal(got, "msg_recv_1") {
		t.Fatalf("mail_search's default view now shows sent mail = %v", got)
	}

	// The input has no folder to override.
	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "mail_sent_search" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(raw, &schema)
		if _, ok := schema.Properties["folder"]; ok {
			t.Fatal("mail_sent_search accepts a folder, so it could return received mail")
		}
		if !tool.Annotations.ReadOnlyHint {
			t.Fatal("mail_sent_search is not marked read-only")
		}
		return
	}
	t.Fatal("mail_sent_search is not listed")
}

// Listing sent mail is searching mail: the same scope as mail_search.
func TestMCPSentSearchRequiresTheSearchScope(t *testing.T) {
	app, ctx, _ := convergenceApp(t)
	_, key, err := app.CreateMCPKeyWithScopes(ctx, "read only", []string{"mail.read"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL, key)
	requireToolError(t, mcpCall(t, client, "mail_sent_search", map[string]any{}), "insufficient_scope")
}
