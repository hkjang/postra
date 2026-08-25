package webui

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"postra/internal/application"
)

func TestAIPriorityHelpers(t *testing.T) {
	cases := []struct {
		labels []string
		want   string
		reply  bool
	}{
		{[]string{"ai/urgent", "ai/reply-needed"}, "urgent", true},
		{[]string{"work", "ai/high"}, "high", false},
		{[]string{"ai/low"}, "low", false},
		{[]string{"work", "personal"}, "", false}, // untriaged mail shows no badge
		{nil, "", false},
	}
	for _, c := range cases {
		if got := aiPriorityOf(c.labels); got != c.want {
			t.Errorf("aiPriorityOf(%v) = %q, want %q", c.labels, got, c.want)
		}
		if got := aiReplyNeededOf(c.labels); got != c.reply {
			t.Errorf("aiReplyNeededOf(%v) = %v, want %v", c.labels, got, c.reply)
		}
	}
}

// The drafted rule round-trips through the browser as JSON, so a tampered or
// malformed payload must be rejected server-side rather than trusted — the
// same validation a hand-authored rule gets.
func TestRuleCreateRevalidatesBrowserPayload(t *testing.T) {
	app, _ := newTestApp(t)
	h := New(app, "").Handler()
	ctx := application.WithActor(context.Background(), "test")

	tampered := url.Values{"rule_json": {
		`{"name":"tampered","match":"all",
		  "conditions":[{"field":"__drop_table","operator":"contains","value":"x"}],
		  "actions":[{"type":"add_label","value":"X"}]}`}}
	rec := do(t, h, http.MethodPost, "/ui/rules", tampered, nil)
	if rec.Code == http.StatusSeeOther {
		t.Fatal("tampered rule was accepted (expected re-validation failure)")
	}
	if rules, err := app.ListRules(ctx); err != nil || len(rules) != 0 {
		t.Fatalf("tampered rule must not be saved, got %d rules (err=%v)", len(rules), err)
	}

	// Malformed JSON is reported, not crashed on.
	rec = do(t, h, http.MethodPost, "/ui/rules", url.Values{"rule_json": {"not json"}}, nil)
	if rec.Code >= 500 {
		t.Fatalf("malformed payload should not 5xx, got %d", rec.Code)
	}

	// A well-formed rule saves and appears in the list.
	valid := url.Values{"rule_json": {
		`{"name":"뉴스레터 보관","match":"any",
		  "conditions":[{"field":"subject","operator":"contains","value":"newsletter"}],
		  "actions":[{"type":"add_label","value":"Archive"}]}`}}
	if rec = do(t, h, http.MethodPost, "/ui/rules", valid, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("valid rule not saved, status %d", rec.Code)
	}
	rules, err := app.ListRules(ctx)
	if err != nil || len(rules) != 1 || rules[0].Name != "뉴스레터 보관" {
		t.Fatalf("expected the saved rule, got %+v (err=%v)", rules, err)
	}
}
