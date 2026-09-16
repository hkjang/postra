package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"postra/internal/domain"
)

type actionCollectionAI struct {
	spaBrowserAI
	result string
}

func (a actionCollectionAI) Generate(context.Context, domain.GenerationRequest) (domain.GenerationResult, error) {
	return domain.GenerationResult{Text: a.result, Model: "collection-fixture"}, nil
}

func TestActionCardRESTEmptyFilteredAndOwnerCollections(t *testing.T) {
	app := browserTestApp(t, true)
	owner, _ := browserIdentity(t, app, "collection-owner", domain.RoleUser)
	other, _ := browserIdentity(t, app, "collection-other", domain.RoleAdmin)
	handler := New(app, "").Handler()
	assertEmpty := func(path, session string) {
		t.Helper()
		response := browserRequest(handler, "GET", path, session, "", "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("collection status %d", response.Code)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || string(body["cards"]) != "[]" {
			t.Fatalf("cards must be an empty JSON array, got %s", response.Body)
		}
	}
	for _, prefix := range []string{"/api", "/api/v1"} {
		assertEmpty(prefix+"/action-cards", owner)
	}
	ctx := context.Background()
	message := &domain.Message{ID: "collection-source", UserID: "collection-owner", AccountID: "acc-collection-owner", UIDL: "collection-source", RawHash: "collection-source"}
	if err := app.Store.InsertMessage(ctx, message, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.CreateActionCard(ctx, &domain.ActionCard{ID: "collection-action", UserID: message.UserID, MessageID: message.ID, Title: "private owner action", Status: domain.ActionCardPending}); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/api", "/api/v1"} {
		assertEmpty(prefix+"/action-cards?status=done", owner)
		assertEmpty(prefix+"/action-cards", other)
	}
	if response := browserRequest(handler, "GET", "/api/action-cards", "", "", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatal("collection normalization weakened authentication")
	}
}

func TestActionCardRESTExtractionEmptyAndInvalidCollections(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		status       int
	}{
		{"empty", `{"cards":[]}`, http.StatusOK},
		{"null", `{"cards":null}`, http.StatusOK},
		{"blank_titles", `{"cards":[{"title":" "}]}`, http.StatusOK},
		{"invalid_list", `{"cards":"not-a-list"}`, http.StatusBadRequest},
		{"invalid_item", `{"cards":[false]}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := browserTestApp(t, true)
			app.AI = actionCollectionAI{result: tc.result}
			owner, csrf := browserIdentity(t, app, "extraction-owner", domain.RoleUser)
			message := &domain.Message{ID: "extraction-source", UserID: "extraction-owner", AccountID: "acc-extraction-owner", UIDL: "extraction-source", RawHash: "extraction-source", Subject: "Empty extraction"}
			if err := app.Store.InsertMessage(context.Background(), message, &domain.MessageBody{MessageID: message.ID, TextBody: "A simple greeting"}, nil); err != nil {
				t.Fatal(err)
			}
			handler := New(app, "").Handler()
			response := browserRequest(handler, "POST", "/api/v1/messages/"+message.ID+"/action-cards", owner, csrf, "https://postra.test", `{}`)
			if response.Code != tc.status {
				t.Fatalf("extraction status %d, want %d: %s", response.Code, tc.status, response.Body)
			}
			if tc.status == http.StatusOK {
				var body map[string]json.RawMessage
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || string(body["cards"]) != "[]" || string(body["count"]) != "0" {
					t.Fatalf("empty extraction contract: %s", response.Body)
				}
			}
		})
	}
}
