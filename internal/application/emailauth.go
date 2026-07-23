package application

import (
	"context"
	"regexp"
	"strings"

	"postra/internal/domain"
)

var (
	reAuthSPF        = regexp.MustCompile(`(?i)\bspf=(\w+)`)
	reAuthDKIM       = regexp.MustCompile(`(?i)\bdkim=(\w+)`)
	reAuthDMARC      = regexp.MustCompile(`(?i)\bdmarc=(\w+)`)
	reAuthARC        = regexp.MustCompile(`(?i)\barc=(\w+)`)
	reAuthMailFrom   = regexp.MustCompile(`(?i)smtp\.mailfrom=([^\s;]+)`)
	reAuthHeaderD    = regexp.MustCompile(`(?i)header\.d=([^\s;]+)`)
	reAuthHeaderFrom = regexp.MustCompile(`(?i)header\.from=([^\s;]+)`)
)

// InspectAuthentication returns the structured SPF/DKIM/DMARC/ARC verdicts,
// domain alignment, and a sender-domain risk score for a stored message.
func (a *App) InspectAuthentication(ctx context.Context, messageID string) (*domain.EmailAuthResult, error) {
	m, err := a.Store.GetMessage(ctx, userIDFrom(ctx), messageID)
	if err != nil {
		return nil, err
	}
	res := ParseAuthResults(m.AuthResults, m.From.Email)
	a.audit(ctx, "auth_inspect", "message:"+messageID, "ok", res.RiskLevel)
	return &res, nil
}

// ParseAuthResults interprets an Authentication-Results header value (as stored
// on the message) against the From address. It is deterministic and offline —
// no DNS lookups — because it reads the receiving MTA's recorded verdicts.
func ParseAuthResults(header, fromEmail string) domain.EmailAuthResult {
	res := domain.EmailAuthResult{
		SPF: "none", DKIM: "none", DMARC: "none",
		FromDomain: authDomain(fromEmail),
		Raw:        strings.TrimSpace(header),
	}
	if m := reAuthSPF.FindStringSubmatch(header); m != nil {
		res.SPF = strings.ToLower(m[1])
	}
	if m := reAuthDKIM.FindStringSubmatch(header); m != nil {
		res.DKIM = strings.ToLower(m[1])
	}
	if m := reAuthDMARC.FindStringSubmatch(header); m != nil {
		res.DMARC = strings.ToLower(m[1])
	}
	if m := reAuthARC.FindStringSubmatch(header); m != nil {
		res.ARC = strings.ToLower(m[1])
	}
	if m := reAuthMailFrom.FindStringSubmatch(header); m != nil {
		res.SPFDomain = authDomain(m[1])
	}
	if m := reAuthHeaderD.FindStringSubmatch(header); m != nil {
		res.DKIMDomain = strings.ToLower(strings.TrimSpace(m[1]))
	}
	if res.FromDomain == "" {
		if m := reAuthHeaderFrom.FindStringSubmatch(header); m != nil {
			res.FromDomain = authDomain(m[1])
		}
	}

	// Alignment: a DKIM/SPF authenticated domain matches (or is a parent of)
	// the visible From domain.
	res.Aligned = domainAligned(res.DKIMDomain, res.FromDomain) || domainAligned(res.SPFDomain, res.FromDomain)

	score := 0
	add := func(pts int, reason string) {
		score += pts
		res.Reasons = append(res.Reasons, reason)
	}
	switch res.SPF {
	case "fail":
		add(25, "SPF hard fail")
	case "softfail":
		add(15, "SPF softfail")
	case "none", "neutral", "":
		add(10, "no SPF pass")
	case "temperror", "permerror":
		add(10, "SPF "+res.SPF)
	}
	switch res.DKIM {
	case "fail":
		add(30, "DKIM signature failed")
	case "none", "":
		add(10, "message not DKIM-signed")
	}
	switch res.DMARC {
	case "fail":
		add(35, "DMARC failed (spoofing risk)")
	case "none", "":
		add(10, "no DMARC policy result")
	}
	if !res.Aligned && (res.DKIMDomain != "" || res.SPFDomain != "") && res.FromDomain != "" {
		add(15, "authenticated domain does not align with From domain")
	}
	if header == "" {
		add(20, "no Authentication-Results header present")
	}
	if score > 100 {
		score = 100
	}
	res.RiskScore = score
	switch {
	case score >= 60:
		res.RiskLevel = "high"
	case score >= 30:
		res.RiskLevel = "medium"
	default:
		res.RiskLevel = "low"
	}
	return res
}

// authDomain extracts a lowercase domain from an address or a bare domain,
// tolerating angle brackets found in smtp.mailfrom / header.from tokens.
func authDomain(addr string) string {
	addr = strings.Trim(strings.TrimSpace(addr), "<>")
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		addr = addr[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(addr))
}

// domainAligned reports whether authDomain equals or is a subdomain-relaxed
// match of fromDomain (RFC 7489 relaxed alignment).
func domainAligned(authDomain, fromDomain string) bool {
	if authDomain == "" || fromDomain == "" {
		return false
	}
	if authDomain == fromDomain {
		return true
	}
	return strings.HasSuffix(authDomain, "."+fromDomain) || strings.HasSuffix(fromDomain, "."+authDomain)
}
