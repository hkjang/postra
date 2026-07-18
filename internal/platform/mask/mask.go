// Package mask redacts personal data and secrets from text. It is applied
// before mail content is sent to an external LLM (AI-011) and, optionally, to
// search/read results (§7 보안 검색). Detection is heuristic and conservative;
// it favors over-redaction of clearly sensitive shapes.
package mask

import (
	"fmt"
	"regexp"
	"sort"
)

type rule struct {
	name string
	re   *regexp.Regexp
}

// Rules are applied in order; earlier rules win on overlapping matches
// (e.g. an RRN is redacted before the generic card/phone rules can split it).
var rules = []rule{
	// Bearer tokens and common API-key shapes.
	{"SECRET", regexp.MustCompile(`(?i)\b(?:bearer\s+[A-Za-z0-9._\-]{16,}|sk-[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{10,})`)},
	// Email addresses.
	{"EMAIL", regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
	// Korean resident registration number (주민등록번호): 6 digits - 7 digits.
	{"RRN", regexp.MustCompile(`\b\d{6}[\-\s]?\d{7}\b`)},
	// US SSN.
	{"SSN", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	// Payment card numbers: 13–19 digits, optionally grouped.
	{"CARD", regexp.MustCompile(`\b(?:\d[ \-]?){13,19}\b`)},
	// Phone numbers (international or local, 8+ digits).
	{"PHONE", regexp.MustCompile(`\+?\d[\d \-]{7,}\d`)},
}

// Hit summarizes redactions of one type.
type Hit struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// Mask replaces sensitive substrings with [REDACTED-<TYPE>] and reports how
// many of each type were redacted.
func Mask(text string) (string, []Hit) {
	counts := map[string]int{}
	for _, r := range rules {
		text = r.re.ReplaceAllStringFunc(text, func(m string) string {
			counts[r.name]++
			return fmt.Sprintf("[REDACTED-%s]", r.name)
		})
	}
	if len(counts) == 0 {
		return text, nil
	}
	hits := make([]Hit, 0, len(counts))
	for t, c := range counts {
		hits = append(hits, Hit{Type: t, Count: c})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Type < hits[j].Type })
	return text, hits
}

// HasSensitive reports whether text contains any detectable sensitive data.
func HasSensitive(text string) bool {
	for _, r := range rules {
		if r.re.MatchString(text) {
			return true
		}
	}
	return false
}
