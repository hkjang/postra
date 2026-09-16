package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"postra/internal/application"
	"postra/internal/domain"
)

type emptyActionAI struct{ outputContractAI }

func (*emptyActionAI) Generate(context.Context, domain.GenerationRequest) (domain.GenerationResult, error) {
	return domain.GenerationResult{Text: `{"cards":null}`, Model: "empty-action-fixture"}, nil
}

func TestMCPActionCardsEmptyCollections(t *testing.T) {
	app, ctx, _ := convergenceApp(t)
	app.AI = &emptyActionAI{}
	owner, _ := application.PrincipalFrom(ctx)
	message := &domain.Message{ID: "empty-action-source", UserID: owner.UserID, AccountID: "acc_mcp", UIDL: "empty-action-source", RawHash: "empty-action-source", Subject: "No tasks"}
	if err := app.Store.InsertMessage(ctx, message, &domain.MessageBody{MessageID: message.ID, TextBody: "A simple greeting"}, nil); err != nil {
		t.Fatal(err)
	}
	_, key, err := app.CreateMCPKeyWithScopes(ctx, "action collection contract", []string{"mail.read", "mail.ai", "mail.work"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL, key)
	assertEmpty := func(tool string, args any) {
		t.Helper()
		result := mcpDecode[map[string]json.RawMessage](t, mcpCall(t, client, tool, args))
		if string(result["cards"]) != "[]" {
			t.Fatalf("%s returned nullable cards: %s", tool, result["cards"])
		}
	}
	assertEmpty("mail_action_cards_list", map[string]any{})
	assertEmpty("mail_action_cards_extract", map[string]any{"message_id": message.ID})
	if err := app.Store.CreateActionCard(ctx, &domain.ActionCard{ID: "action-pending", UserID: owner.UserID, MessageID: message.ID, Title: "Pending action", Status: domain.ActionCardPending}); err != nil {
		t.Fatal(err)
	}
	assertEmpty("mail_action_cards_list", map[string]any{"status": "done"})
}
