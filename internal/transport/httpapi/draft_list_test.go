package httpapi

import (
	"context"
	"encoding/json"
	"net/url"
	"postra/internal/application"
	"postra/internal/domain"
	"strings"
	"testing"
)

func TestDraftListPersistencePaginationAndPrivacy(t *testing.T) {
	app := browserTestApp(t, true)
	admin, csrf := browserIdentity(t, app, "draft-admin", domain.RoleAdmin)
	browserIdentity(t, app, "draft-other", domain.RoleUser)
	for _, entry := range []struct{ id, owner, subject string }{{"draft-a", "draft-admin", "초안 A"}, {"draft-b", "draft-admin", "초안 B"}, {"draft-z", "draft-other", "다른 사용자 비밀 제목"}} {
		d := &domain.Draft{ID: entry.id, UserID: entry.owner, AccountID: "acc-" + entry.owner, Kind: domain.DraftNew, Status: domain.DraftOpen}
		v := &domain.DraftVersion{Subject: entry.subject, BodyText: "목록에 전송하지 않는 본문", Author: "user", Bcc: []domain.Address{{Email: "private-bcc@corp.local"}}}
		if err := app.Store.CreateDraft(context.Background(), d, v); err != nil {
			t.Fatal(err)
		}
	}
	h := New(app, "").Handler()
	first := browserRequest(h, "GET", "/api/drafts?limit=1", admin, "", "", "")
	if first.Code != 200 || strings.Contains(first.Body.String(), "비밀") || strings.Contains(first.Body.String(), "본문") || strings.Contains(first.Body.String(), "private-bcc") {
		t.Fatalf("draft list leaked private data: %d %s", first.Code, first.Body.String())
	}
	var page application.DraftList
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || len(page.Drafts) != 1 || page.NextCursor == "" {
		t.Fatalf("first page: %s %v", first.Body.String(), err)
	}
	firstID := page.Drafts[0].ID
	second := browserRequest(h, "GET", "/api/drafts?limit=1&cursor="+url.QueryEscape(page.NextCursor), admin, "", "", "")
	page = application.DraftList{}
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil || second.Code != 200 || len(page.Drafts) != 1 || page.Drafts[0].ID == firstID || page.NextCursor != "" {
		t.Fatalf("second page: %s %v", second.Body.String(), err)
	}
	if w := browserRequest(h, "DELETE", "/api/drafts/"+firstID, admin, csrf, "https://postra.test", ""); w.Code != 204 {
		t.Fatalf("discard failed: %d %s", w.Code, w.Body.String())
	}
	if w := browserRequest(h, "DELETE", "/api/drafts/draft-z", admin, csrf, "https://postra.test", ""); w.Code != 404 {
		t.Fatalf("admin discarded someone else's draft: %d", w.Code)
	}
	current := browserRequest(New(app, "").Handler(), "GET", "/api/drafts", admin, "", "", "")
	if err := json.Unmarshal(current.Body.Bytes(), &page); err != nil || len(page.Drafts) != 1 || page.Drafts[0].ID == firstID {
		t.Fatalf("fresh handler list did not preserve server state: %s", current.Body.String())
	}
	for _, query := range []string{"cursor=malformed", "status=invalid"} {
		if w := browserRequest(h, "GET", "/api/drafts?"+query, admin, "", "", ""); w.Code != 400 {
			t.Fatalf("invalid filter accepted: %s => %d", query, w.Code)
		}
	}
}
