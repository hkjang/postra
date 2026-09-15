package httpapi

import (
	"context"
	"encoding/json"
	"postra/internal/domain"
	"strings"
	"testing"
)

func TestUnreadStateAndSnippetsAreOwnerScoped(t *testing.T) {
	app := browserTestApp(t, true)
	admin, csrf := browserIdentity(t, app, "read-admin", domain.RoleAdmin)
	browserIdentity(t, app, "read-other", domain.RoleUser)
	for _, owner := range []string{"read-admin", "read-other"} {
		m := &domain.Message{ID: "msg-" + owner, UserID: owner, AccountID: "acc-" + owner, UIDL: owner, Subject: owner, From: domain.Address{Email: "sender@corp.local"}, Date: 1700000000, RawHash: owner, RawURI: "local://fixture"}
		body := &domain.MessageBody{MessageID: m.ID, TextBody: owner + " 프리뷰 본문입니다."}
		if err := app.Store.InsertMessage(context.Background(), m, body, nil); err != nil {
			t.Fatal(err)
		}
	}
	h := New(app, "").Handler()
	get := browserRequest(h, "GET", "/api/messages/msg-read-admin", admin, "", "", "")
	if get.Code != 200 || !strings.Contains(get.Body.String(), `"is_read":false`) {
		t.Fatalf("new message is not unread: %d %s", get.Code, get.Body.String())
	}
	m, err := app.Store.GetMessage(context.Background(), "read-admin", "msg-read-admin")
	if err != nil || m.IsRead {
		t.Fatal("GET must not mutate read state")
	}
	result := browserRequest(h, "GET", "/api/messages?is_read=false", admin, "", "", "")
	var list domain.SearchResult
	if err := json.Unmarshal(result.Body.Bytes(), &list); err != nil || len(list.Messages) != 1 || !strings.Contains(list.Snippets["msg-read-admin"], "프리뷰") || strings.Contains(result.Body.String(), "read-other") {
		t.Fatalf("unread/snippet leak: %s", result.Body.String())
	}
	changed := browserRequest(h, "POST", "/api/messages/batch", admin, csrf, "https://postra.test", `{"message_ids":["msg-read-admin","msg-read-other"],"action":"mark_read"}`)
	if changed.Code != 200 || !strings.Contains(changed.Body.String(), `"failed":1`) || !strings.Contains(changed.Body.String(), `"succeeded":1`) {
		t.Fatalf("read isolation failed: %s", changed.Body.String())
	}
	result = browserRequest(h, "GET", "/api/messages?is_read=false", admin, "", "", "")
	if strings.Contains(result.Body.String(), "msg-read-admin") {
		t.Fatal("read message still matches unread filter")
	}
	other, err := app.Store.GetMessage(context.Background(), "read-other", "msg-read-other")
	if err != nil || other.IsRead {
		t.Fatal("administrator changed another user's read state")
	}
	changed = browserRequest(h, "POST", "/api/messages/batch", admin, csrf, "https://postra.test", `{"message_ids":["msg-read-admin"],"action":"mark_unread"}`)
	if changed.Code != 200 {
		t.Fatalf("mark unread: %d", changed.Code)
	}
	result = browserRequest(New(app, "").Handler(), "GET", "/api/messages?is_read=false", admin, "", "", "")
	if !strings.Contains(result.Body.String(), "msg-read-admin") {
		t.Fatal("read state was not persisted across handler recreation")
	}
}
