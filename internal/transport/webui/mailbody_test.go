package webui

import "strings"

import "testing"

func TestRenderMailTextEscapesAndFormats(t *testing.T) {
	got := string(renderMailText(
		"Hello <script>alert(1)</script> & \"world\"\n" +
			"Visit http://example.com/x?a=1&b=2 now.\n" +
			"Mail me at a.b@corp.local\n" +
			"\n" +
			"> quoted reply\n" +
			"> second quoted line\n" +
			"**bold** and `code`"))

	mustContain := map[string]string{
		"escapes script":     "&lt;script&gt;alert(1)&lt;/script&gt;",
		"escapes ampersand":  "&amp;",
		"autolinks url":      `<a href="http://example.com/x?a=1&amp;b=2"`,
		"autolinks email":    `<a href="mailto:a.b@corp.local">`,
		"quote block":        "<blockquote>",
		"quote joined by br": "quoted reply<br>second quoted line",
		"bold":               "<strong>bold</strong>",
		"code":               "<code>code</code>",
	}
	for name, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("%s: output missing %q\n---\n%s", name, want, got)
		}
	}
	// Critical: no live script tag survives.
	if strings.Contains(got, "<script>") {
		t.Fatalf("unescaped <script> in output: %s", got)
	}
}

func TestRenderMailTextTrailingPunctuationNotInLink(t *testing.T) {
	got := string(renderMailText("see (http://example.com)."))
	if !strings.Contains(got, `<a href="http://example.com"`) {
		t.Fatalf("URL not linked cleanly: %s", got)
	}
	if strings.Contains(got, `example.com).`) && strings.Contains(got, `href="http://example.com).`) {
		t.Fatalf("trailing punctuation leaked into href: %s", got)
	}
}
