package application

import (
	"strings"

	"postra/internal/platform/mask"
)

// DLPFinding is one category of sensitive content detected in outbound text.
type DLPFinding struct {
	Type  string `json:"type"`  // PII/secret shape (RRN, CARD, SECRET, ...) or "KEYWORD"
	Count int    `json:"count"` // occurrences
	// Term is set for KEYWORD findings (the matched policy term).
	Term string `json:"term,omitempty"`
}

// scanDLP inspects outbound subject+body for PII/secret shapes (reusing the
// mask detectors) and configured policy keywords. It never returns the
// sensitive values themselves — only categories and counts.
func (a *App) scanDLP(subject, body string) []DLPFinding {
	text := subject + "\n" + body
	var out []DLPFinding
	if _, hits := mask.Mask(text); len(hits) > 0 {
		for _, h := range hits {
			// EMAIL/PHONE are ubiquitous in legitimate mail; do not treat them
			// as DLP violations (they are still masked for external AI).
			if h.Type == "EMAIL" || h.Type == "PHONE" {
				continue
			}
			out = append(out, DLPFinding{Type: h.Type, Count: h.Count})
		}
	}
	lower := strings.ToLower(text)
	for _, kw := range a.Cfg.Send.DLPKeywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		if n := strings.Count(lower, strings.ToLower(kw)); n > 0 {
			out = append(out, DLPFinding{Type: "KEYWORD", Count: n, Term: kw})
		}
	}
	return out
}

// dlpPolicy returns the effective DLP policy ("off"|"warn"|"block").
func (a *App) dlpPolicy() string {
	p := strings.ToLower(strings.TrimSpace(a.Cfg.Send.DLPPolicy))
	switch p {
	case "off", "warn", "block":
		return p
	default:
		return "warn"
	}
}
