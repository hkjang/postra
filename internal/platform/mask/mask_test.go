package mask

import (
	"strings"
	"testing"
)

func TestMaskPatterns(t *testing.T) {
	cases := []struct {
		in       string
		wantType string
		wantGone string
	}{
		{"연락처 bob@example.com 입니다", "EMAIL", "bob@example.com"},
		{"주민번호 900101-1234567", "RRN", "900101-1234567"},
		{"card 4111 1111 1111 1111 please", "CARD", "4111 1111 1111 1111"},
		{"call +82 10-1234-5678 now", "PHONE", "1234-5678"},
		{"token sk-abcdefghijklmnopqrstuvwx used", "SECRET", "sk-abcdefghijklmnopqrstuvwx"},
		{"ssn 123-45-6789 here", "SSN", "123-45-6789"},
	}
	for _, c := range cases {
		out, hits := Mask(c.in)
		if strings.Contains(out, c.wantGone) {
			t.Errorf("%q: sensitive substring %q survived: %q", c.in, c.wantGone, out)
		}
		found := false
		for _, h := range hits {
			if h.Type == c.wantType {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: expected a %s hit, got %v", c.in, c.wantType, hits)
		}
	}
}

func TestMaskCleanTextUnchanged(t *testing.T) {
	in := "Let's meet on Tuesday to discuss the roadmap."
	out, hits := Mask(in)
	if out != in || hits != nil {
		t.Fatalf("clean text changed: %q hits=%v", out, hits)
	}
}

func TestHasSensitive(t *testing.T) {
	if !HasSensitive("email a@b.com") {
		t.Error("should detect email")
	}
	if HasSensitive("nothing here") {
		t.Error("false positive on clean text")
	}
}
