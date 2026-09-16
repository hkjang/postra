package notifymail

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreOffAndShapedForAnInternalRelay(t *testing.T) {
	c := ConfigFromValues(nil)
	if c.Enabled || c.Port != 25 || c.Security != "auto" || c.Username != "" || c.PasswordRef != "" || c.Timeout != 10*time.Second || c.SkipVerify {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	for _, event := range Events {
		if !c.Allows(event) {
			t.Fatalf("event %s should be on by default", event)
		}
	}
	if !c.Allows("something_new") {
		t.Fatal("unknown events are sent so a new notification never needs a settings change")
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), KeyHost) {
		t.Fatalf("empty host must be named as the reason: %v", err)
	}
}

func TestConfigFromValues(t *testing.T) {
	c := ConfigFromValues(map[string]string{
		KeyEnabled: "true", KeyHost: " relay.corp.local ", KeyPort: "465", KeyPassword: "sec_abc",
		KeyTimeout: "0", NotifyKey(EventAssigned): "false", KeyBaseURL: "https://postra.corp.local/",
	})
	if !c.Enabled || c.Host != "relay.corp.local" || c.Security != "tls" || c.PasswordRef != "sec_abc" || c.Timeout != 10*time.Second {
		t.Fatalf("parsed config wrong: %+v", c)
	}
	if c.Allows(EventAssigned) || !c.Allows(EventSendFailed) {
		t.Fatal("per-event switch must affect only its event")
	}
	if c.FromAddress != "postra@relay.corp.local" {
		t.Fatalf("missing from address should fall back to the relay host, got %q", c.FromAddress)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.Link("/app/mail/1"); got != "https://postra.corp.local/app/mail/1" {
		t.Fatalf("link = %q", got)
	}
	c.Security = "ssl"
	if err := c.Validate(); err == nil {
		t.Fatal("unknown security must be rejected")
	}
}

func TestComposeEncodesKoreanAndUsesCRLF(t *testing.T) {
	c := ConfigFromValues(map[string]string{KeyHost: "relay", KeyFromAddress: "noreply@corp.local", KeyFromName: "포스트라 알림"})
	n := Assigned("홍길동", "견적 요청\r\nBcc: evil@x", "msg")
	raw := string(Compose(c, "to@corp.local", n.Subject, n.Body(c), time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)))
	head, body, _ := strings.Cut(raw, "\r\n\r\n")
	if strings.Contains(head, "\r\nBcc:") || strings.Count(head, "\r\n") != 8 {
		t.Fatalf("subject injection reached the headers:\n%s", head)
	}
	for _, want := range []string{"From: =?utf-8?q?", "<noreply@corp.local>", "Subject: =?utf-8?q?", "Auto-Submitted: auto-generated", "Content-Type: text/plain; charset=UTF-8"} {
		if !strings.Contains(head, want) {
			t.Fatalf("missing header %q in:\n%s", want, head)
		}
	}
	if strings.Contains(body, "\n") && !strings.Contains(body, "\r\n") || !strings.HasSuffix(body, "\r\n") {
		t.Fatalf("body must use CRLF line endings:\n%q", body)
	}
	if strings.Contains(body, "바로 열기") {
		t.Fatal("no base URL means no link")
	}
	if !strings.Contains(body, "홍길동 님이") || !strings.Contains(body, "자동으로 발송") {
		t.Fatalf("body text unexpected:\n%s", body)
	}
}

func TestSLADueBundlesOverdueAndImminentIntoOneMail(t *testing.T) {
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	single := SLADue([]SLAItem{{MessageID: "m1", Subject: "견적 요청", Due: now.Add(2 * time.Hour)}}, now)
	if single.Event != EventSLADue || !strings.Contains(single.Subject, "임박했습니다: 견적 요청") || single.Path != "/app/team?message=m1" {
		t.Fatalf("single imminent: %+v", single)
	}
	if body := strings.Join(single.Lines, "\n"); !strings.Contains(body, "24시간 안에 기한이 되는 담당 메일 (1건)") || !strings.Contains(body, "기한 2026-09-16 11:00") || strings.Contains(body, "지난") {
		t.Fatalf("single imminent body:\n%s", body)
	}
	missed := SLADue([]SLAItem{{MessageID: "m1", Subject: "", Due: now.Add(-time.Minute)}}, now)
	if !strings.Contains(missed.Subject, "지났습니다: (제목 없음)") {
		t.Fatalf("single missed: %+v", missed)
	}

	var items []SLAItem
	for i := 0; i < 23; i++ {
		items = append(items, SLAItem{MessageID: "o" + string(rune('a'+i)), Subject: "late " + string(rune('a'+i)), Due: now.Add(-time.Duration(i+1) * time.Hour)})
	}
	items = append(items, SLAItem{MessageID: "s1", Subject: "soon", Due: now.Add(time.Hour)})
	mixed := SLADue(items, now)
	if mixed.Subject != "[Postra] 담당 메일 기한: 지남 23건, 임박 1건" || mixed.Path != "/app/team" {
		t.Fatalf("mixed: %+v", mixed)
	}
	body := strings.Join(mixed.Lines, "\n")
	for _, want := range []string{"기한이 지난 담당 메일 (23건)", "… 외 3건", "24시간 안에 기한이 되는 담당 메일 (1건)", "- soon —"} {
		if !strings.Contains(body, want) {
			t.Fatalf("mixed body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "late v") || !strings.Contains(body, "late t") {
		t.Fatalf("only the first 20 overdue items are listed:\n%s", body)
	}
}
