package handoff

import (
	"strings"
	"testing"
)

func TestParseTargetsEmptyByDefault(t *testing.T) {
	for _, raw := range []string{"", "  ", Defaults[SettingTargets]} {
		targets, err := ParseTargets(raw)
		if err != nil || len(targets) != 0 {
			t.Fatalf("%q: targets=%v err=%v; a fresh installation has no targets", raw, targets, err)
		}
	}
}

func TestParseTargetsNormalisesAndFilters(t *testing.T) {
	raw := `[
	  {"name":" Ptium ","origin":"HTTPS://Ptium.Intra/","formats":["Markdown","docx"," "]},
	  {"name":"Kanpic","origin":"https://kanpic.intra:8443","formats":["csv","xlsx"]}
	]`
	targets, err := ParseTargets(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].Name != "Ptium" || targets[0].Origin != "https://ptium.intra" {
		t.Fatalf("unexpected targets: %+v", targets)
	}
	if got := strings.Join(targets[0].Formats, ","); got != "markdown,docx" {
		t.Fatalf("formats not normalised: %q", got)
	}
	if targets[1].Origin != "https://kanpic.intra:8443" {
		t.Fatalf("port must survive: %q", targets[1].Origin)
	}
	// Only services that read markdown get a button.
	sending := Accepting(targets, FormatMarkdown)
	if len(sending) != 1 || sending[0].Name != "Ptium" {
		t.Fatalf("Accepting(markdown) = %+v", sending)
	}
}

func TestParseTargetsRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"not json":       `{"name":"x"}`,
		"path in origin": `[{"name":"A","origin":"https://a.intra/handoff","formats":["markdown"]}]`,
		"query":          `[{"name":"A","origin":"https://a.intra/?x=1","formats":["markdown"]}]`,
		"credentials":    `[{"name":"A","origin":"https://u:p@a.intra","formats":["markdown"]}]`,
		"scheme":         `[{"name":"A","origin":"ftp://a.intra","formats":["markdown"]}]`,
		"no name":        `[{"name":"","origin":"https://a.intra","formats":["markdown"]}]`,
		"no formats":     `[{"name":"A","origin":"https://a.intra","formats":[]}]`,
		"unknown format": `[{"name":"A","origin":"https://a.intra","formats":["pdf"]}]`,
		"duplicate":      `[{"name":"A","origin":"https://a.intra","formats":["markdown"]},{"name":"B","origin":"https://A.intra/","formats":["docx"]}]`,
	}
	for name, raw := range cases {
		if _, err := ParseTargets(raw); err == nil {
			t.Errorf("%s: %s should be rejected", name, raw)
		}
	}
	var many []string
	for i := 0; i <= MaxTargets; i++ {
		many = append(many, `{"name":"S","origin":"https://s`+strings.Repeat("x", i)+`.intra","formats":["markdown"]}`)
	}
	if _, err := ParseTargets("[" + strings.Join(many, ",") + "]"); err == nil {
		t.Errorf("more than %d targets should be rejected", MaxTargets)
	}
}

func TestClaimTokenShape(t *testing.T) {
	token, digest, err := NewClaim()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidClaimToken(token) {
		t.Fatalf("fresh token should be valid: %q", token)
	}
	if Digest(token) != digest || len(digest) != 64 || digest == token {
		t.Fatalf("digest must be the hex sha-256 of the token")
	}
	other, _, _ := NewClaim()
	if other == token {
		t.Fatal("two claims must differ")
	}
	for _, bad := range []string{"", "abc", token + "x", token[:len(token)-1] + "!", strings.Repeat("A", 200)} {
		if ValidClaimToken(bad) {
			t.Errorf("%q should not pass as a claim", bad)
		}
	}
}

func TestURLIsTheStandardShape(t *testing.T) {
	got := URL(Target{Origin: "https://ptium.intra"}, "https://postra.intra", "abc_-1")
	if got != "https://ptium.intra/handoff?claim=abc_-1&source=https%3A%2F%2Fpostra.intra" {
		t.Fatalf("URL = %s", got)
	}
}

func TestFilenameAndDisposition(t *testing.T) {
	cases := map[string]string{
		"":                        "메일.md",
		"  2026년 3분기 개편안  ":       "2026년 3분기 개편안.md",
		`Re: a/b\c:d*e?f"g<h>i|j`: "Re_ a_b_c_d_e_f_g_h_i_j.md",
		"line\nbreak\x00":         "linebreak.md",
		strings.Repeat("가", 100):  strings.Repeat("가", 40) + ".md",
		"..":                      "메일.md",
	}
	for subject, want := range cases {
		if got := Filename(subject); got != want {
			t.Errorf("Filename(%q) = %q, want %q", subject, got, want)
		}
	}
	if got := ContentDisposition("2026년 개편안.md"); got != "attachment; filename*=UTF-8''2026%EB%85%84%20%EA%B0%9C%ED%8E%B8%EC%95%88.md" {
		t.Fatalf("ContentDisposition = %s", got)
	}
}
