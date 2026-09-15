package httpapi

import (
	"postra/internal/domain"
	"strings"
	"testing"
)

func TestBrowserRulePayloadIsRevalidated(t *testing.T) {
	app := browserTestApp(t, true)
	session, csrf := browserIdentity(t, app, "rule-owner", domain.RoleUser)
	h := New(app, "").Handler()
	for _, body := range []string{`{"name":"tampered","match":"all","conditions":[{"field":"__drop_table","operator":"contains","value":"x"}],"actions":[{"type":"add_label","value":"X"}]}`, `not json`} {
		response := browserRequest(h, "POST", "/api/rules", session, csrf, "https://postra.test", body)
		if response.Code != 400 {
			t.Fatalf("invalid rule was not safely rejected: %d %s", response.Code, response.Body.String())
		}
	}
	if response := browserRequest(h, "GET", "/api/rules", session, "", "", ""); strings.Contains(response.Body.String(), "tampered") {
		t.Fatal("tampered rule was persisted")
	}
	body := `{"name":"뉴스레터 보관","match":"any","conditions":[{"field":"subject","operator":"contains","value":"newsletter"}],"actions":[{"type":"add_label","value":"Archive"}]}`
	if response := browserRequest(h, "POST", "/api/rules", session, csrf, "https://postra.test", body); response.Code != 200 {
		t.Fatalf("valid rule: %d %s", response.Code, response.Body.String())
	}
	if response := browserRequest(h, "GET", "/api/rules", session, "", "", ""); !strings.Contains(response.Body.String(), "뉴스레터 보관") {
		t.Fatal("valid rule was not listed")
	}
}
