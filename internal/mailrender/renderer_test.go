package mailrender

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"postra/internal/platform/mailhtml"
)

func TestFragmentGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("golden fixtures: %v (%d files)", err, len(files))
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Input  Input  `json:"input"`
				HTML   string `json:"html"`
				Text   string `json:"text"`
				Format string `json:"format"`
			}
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			out, err := Fragment(fixture.Input)
			if err != nil {
				t.Fatal(err)
			}
			if out.BodyHTML != fixture.HTML || out.BodyText != fixture.Text || out.Format != fixture.Format {
				t.Fatalf("golden mismatch\nHTML: %q\nText: %q\nFormat: %q", out.BodyHTML, out.BodyText, out.Format)
			}
		})
	}
}

func TestTemplatesAreSafeDeterministicAndRepeatable(t *testing.T) {
	for _, template := range Templates() {
		t.Run(template.ID, func(t *testing.T) {
			in := Input{Body: "# 진행 보고\n\n안녕하세요 **동료** 여러분.\n\n- 완료\n- 검토\n\n| 항목 | 현황 |\n| --- | --- |\n| 일정 | 정상 |", Format: "auto", Template: template.ID}
			out, err := Render(in)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := Render(in)
			if out.BodyHTML != again.BodyHTML || out.BodyText != again.BodyText {
				t.Fatal("nondeterministic render")
			}
			if template.ID == "plain" {
				if out.BodyHTML != "" || strings.Contains(out.BodyText, "**") {
					t.Fatalf("plain template: %+v", out)
				}
				return
			}
			if !strings.Contains(out.BodyHTML, `style="`) || strings.Contains(out.BodyHTML, "<style") || strings.Contains(out.BodyHTML, "<link") {
				t.Fatal("theme must be entirely inline")
			}
			if !strings.Contains(out.BodyHTML, `<h1 style="`) || !strings.Contains(out.BodyHTML, `<th style="`) {
				t.Fatal("theme must style headings and tables, not only its wrapper")
			}
			if out.BodyText != mailhtml.PlainText(out.BodyHTML) {
				t.Fatal("text is not canonical final HTML")
			}
			rerendered, err := Render(Input{BodyHTML: out.BodyHTML, Format: "auto", Template: template.ID})
			if err != nil || rerendered.BodyHTML != out.BodyHTML || rerendered.BodyText != out.BodyText {
				t.Fatalf("reapply changed theme: %v\n%s\n%s", err, out.BodyHTML, rerendered.BodyHTML)
			}
		})
	}
}

func TestCleanThemeGolden(t *testing.T) {
	out, err := Render(Input{Body: "# 제목\n\n본문", Format: "markdown", Template: "clean"})
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/clean.golden.html")
	if err != nil {
		t.Fatal(err)
	}
	if out.BodyHTML != strings.TrimSuffix(string(golden), "\n") || out.BodyText != "제목\n본문" {
		t.Fatalf("theme golden mismatch:\n%s\n%q", out.BodyHTML, out.BodyText)
	}
}

func TestChangingTemplateReplacesOldDefaultsButPreservesAuthorStyles(t *testing.T) {
	clean, _ := Render(Input{Body: `<h1>제목</h1><p style="color:#123456">작성자 강조</p>`, Format: "html", Template: "clean"})
	formal, err := Render(Input{BodyHTML: clean.BodyHTML, Format: "html", Template: "formal"})
	if err != nil || !strings.Contains(formal.BodyHTML, "#334155") || strings.Contains(formal.BodyHTML, "#3157d5") || !strings.Contains(formal.BodyHTML, "#123456") {
		t.Fatalf("template replacement: %s %v", formal.BodyHTML, err)
	}
}

func TestSanitizeUntrustedHTMLAndMarkdown(t *testing.T) {
	in := Input{Format: "html", Body: `<h2 onclick="evil()">제목</h2><script>secret()</script><img src="https://tracker.test/pixel"><p style="background-image:url(https://tracker.test/a);position:fixed;color:#123456">본문 <a href="javascript:evil()">링크</a></p><iframe src="https://evil.test"></iframe>`}
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"onclick", "script", "secret()", "<img", "tracker", "javascript:", "<iframe", "position:"} {
		if strings.Contains(out.BodyHTML, bad) {
			t.Fatalf("unsafe HTML %q: %s", bad, out.BodyHTML)
		}
	}
	if !strings.Contains(out.BodyHTML, "#123456") {
		t.Fatal("safe authored styles must survive")
	}
	markdown, _ := Render(Input{Format: "markdown", Body: `<script>alert(1)</script>\n\n[위험](javascript:alert) ![로고](https://tracker.test/logo)\n\n\\한글`})
	if strings.Contains(markdown.BodyHTML, "<script") || strings.Contains(markdown.BodyHTML, "javascript:") || strings.Contains(markdown.BodyHTML, "tracker.test") || !strings.Contains(markdown.BodyText, `\한글`) {
		t.Fatalf("unsafe/corrupted Markdown: %+v", markdown)
	}
}

func TestSignatureReplacementAndRemoval(t *testing.T) {
	in := Input{Body: "안녕하세요", Format: "text", SignatureID: "sig_one", SignatureHTML: `<p>홍길동 <a href="mailto:hong@corp.local">메일</a></p>`}
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Body, in.BodyHTML, in.Format = out.BodyText, out.BodyHTML, "auto"
	again, err := Render(in)
	if err != nil || again.BodyHTML != out.BodyHTML || strings.Count(again.BodyText, "홍길동") != 1 {
		t.Fatalf("signature duplicated: %+v %v", again, err)
	}
	in.SignatureID, in.SignatureHTML = "sig_two", "<p>김철수</p>"
	replaced, _ := Render(in)
	if strings.Contains(replaced.BodyText, "홍길동") || !strings.Contains(replaced.BodyText, "김철수") {
		t.Fatal("old signature was not replaced")
	}
	removed, _ := Render(Input{BodyHTML: replaced.BodyHTML, Format: "auto", StripSignature: true})
	if strings.Contains(removed.BodyText, "김철수") || removed.BodyText != "안녕하세요" {
		t.Fatalf("signature not removed: %+v", removed)
	}
	plain, _ := Render(Input{Body: "A\r\n\r\nB", Format: "text", Template: "plain", SignatureText: "홍길동"})
	if plain.BodyText != "A\n\nB\n\n-- \n홍길동" || plain.BodyHTML != "" {
		t.Fatalf("plain signature: %+v", plain)
	}
}

func TestInvalidInputs(t *testing.T) {
	for _, in := range []Input{{Format: "script"}, {Template: "unknown"}, {Body: strings.Repeat("x", MaxInputBytes+1)}, {Body: strings.Repeat("x", MaxInputBytes), SignatureText: "x"}} {
		if _, err := Render(in); err == nil {
			t.Fatalf("invalid input accepted: format=%s template=%s", in.Format, in.Template)
		}
	}
}

func FuzzRenderSafeCanonical(f *testing.F) {
	f.Add("<script>alert(1)</script><p>Hello</p>", "html")
	f.Add("# 안녕하세요\n\n- 안전", "markdown")
	f.Fuzz(func(t *testing.T, body, format string) {
		if len(body) > 65536 {
			t.Skip()
		}
		out, err := Render(Input{Body: body, Format: format})
		if err != nil {
			return
		}
		if out.BodyText != mailhtml.PlainText(out.BodyHTML) || out.BodyHTML != mailhtml.Sanitize(out.BodyHTML) {
			t.Fatal("renderer is not canonical/idempotently sanitized")
		}
	})
}
