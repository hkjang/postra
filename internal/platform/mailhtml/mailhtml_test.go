package mailhtml

import (
	"strings"
	"testing"
)

func TestSanitizePreservesEmailLayoutWithoutActiveContent(t *testing.T) {
	raw := `<div style="font-family:Arial,sans-serif;max-width:640px;background-color:#f1f5f9;padding:24px;border-radius:12px;position:fixed;background-image:url(https://tracker.invalid/pixel)"><h2 style="color:rgb(30, 64, 175);font-size:26px;line-height:1.6">주간 소식</h2><table style="width:100%;border-collapse:collapse"><tr><td style="border:1px solid #cbd5e1;padding:12px">진행 현황</td></tr></table><p><strong>완료</strong> <a href="https://corp.local/news" onclick="alert(1)">상세 보기</a></p><script>alert('secret')</script><style>body{display:none}</style><img src="https://tracker.invalid/pixel" onerror="alert(1)"><iframe src="https://evil.invalid"></iframe><a href="javascript:alert(1)">bad link</a><form><input value="secret"></form></div>`
	got := Sanitize(raw)
	for _, want := range []string{"font-family: Arial,sans-serif", "max-width: 640px", "background-color: #f1f5f9", "padding: 24px", "border-radius: 12px", "font-size: 26px", "line-height: 1.6", "<table", "border-collapse: collapse", "border: 1px solid #cbd5e1", "<strong>완료</strong>", `href="https://corp.local/news"`} {
		if !strings.Contains(got, want) {
			t.Errorf("lost formatting %q in %s", want, got)
		}
	}
	for _, forbidden := range []string{"<script", "<style", "<img", "<iframe", "<form", "<input", "onclick", "onerror", "javascript:", "position:", "background-image", "tracker.invalid", "secret"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("unsafe content %q remains: %s", forbidden, got)
		}
	}
	if again := Sanitize(got); again != got {
		t.Fatalf("sanitization must be stable: %q != %q", again, got)
	}
}

func TestPlainTextKeepsBlocksUnicodeLinksAndInlineValues(t *testing.T) {
	raw := `<h2>안내 &amp; 소식</h2><p>안녕하세요 <strong>홍길동</strong>님.</p><ul><li>첫째</li><li>둘째</li></ul><table><tr><th>항목</th><th>상태</th></tr><tr><td>개발</td><td>완료</td></tr></table><p><a href="https://corp.local/news?a=1&amp;b=2">자세히</a></p><p><span>900101</span>-<strong>1234567</strong></p><pre>  code` + "\n" + `    indented</pre>`
	got := PlainText(raw)
	for _, want := range []string{"안내 & 소식\n", "안녕하세요 홍길동님.", "- 첫째\n- 둘째", "항목\t상태", "개발\t완료", "자세히 <https://corp.local/news?a=1&b=2>", "900101-1234567", "  code\n    indented"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in plain alternative %q", want, got)
		}
	}
}

func TestSanitizeRejectsUnsafeCSSAndLinks(t *testing.T) {
	for _, raw := range []string{
		`<p style="color:expression(alert(1));background:url(https://evil.invalid);display:none">safe</p>`,
		`<a href="data:text/html,evil">safe</a>`,
		`<a href="&#106;avascript:alert(1)">safe</a>`,
		`<a href="/admin/users">safe</a>`,
		`<svg><a xlink:href="javascript:alert(1)">safe</a></svg>`,
	} {
		got := Sanitize(raw)
		for _, forbidden := range []string{"expression", "url(", "display:", "href=", "<svg"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("unsafe %q retained in %q", forbidden, got)
			}
		}
	}
}
