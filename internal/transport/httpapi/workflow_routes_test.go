package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"postra/internal/domain"
)

func TestManualActionRESTRequiresOwnerAndCSRF(t *testing.T) {
	app := browserTestApp(t, true)
	owner, csrf := browserIdentity(t, app, "action-owner", domain.RoleUser)
	other, otherCSRF := browserIdentity(t, app, "action-other", domain.RoleAdmin)
	if err := app.Store.InsertMessage(context.Background(), &domain.Message{ID: "manual-action-message", UserID: "action-owner", AccountID: "acc-action-owner", UIDL: "action-uidl", Subject: "source", RawHash: "action-hash"}, &domain.MessageBody{MessageID: "manual-action-message", TextBody: "never send this to AI"}, nil); err != nil {
		t.Fatal(err)
	}
	handler := New(app, "").Handler()
	body := `{"message_id":"manual-action-message","title":"내 할 일","due":"2026-09-20"}`
	for _, test := range []struct {
		session, csrf string
		status        int
	}{{owner, "", http.StatusForbidden}, {other, otherCSRF, http.StatusNotFound}, {owner, csrf, http.StatusCreated}} {
		response := browserRequest(handler, "POST", "/api/action-cards", test.session, test.csrf, "https://postra.test", body)
		if response.Code != test.status {
			t.Fatalf("action status %d, want %d: %s", response.Code, test.status, response.Body)
		}
		if response.Code == http.StatusCreated && !strings.Contains(response.Body.String(), `"status":"pending"`) {
			t.Fatal("manual action bypassed review state")
		}
	}
}
