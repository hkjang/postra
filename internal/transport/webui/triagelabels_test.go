package webui

import "testing"

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
