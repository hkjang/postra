package application

import "testing"

func TestParseAuthResultsPass(t *testing.T) {
	h := "mx.google.com; spf=pass smtp.mailfrom=alice@example.com; dkim=pass header.d=example.com; dmarc=pass header.from=example.com"
	r := ParseAuthResults(h, "alice@example.com")
	if r.SPF != "pass" || r.DKIM != "pass" || r.DMARC != "pass" {
		t.Fatalf("verdicts: %+v", r)
	}
	if !r.Aligned {
		t.Fatalf("expected aligned, got %+v", r)
	}
	if r.RiskLevel != "low" {
		t.Fatalf("expected low risk, got %s (score %d)", r.RiskLevel, r.RiskScore)
	}
}

func TestParseAuthResultsSpoof(t *testing.T) {
	h := "mx.example.net; spf=fail smtp.mailfrom=attacker@evil.test; dkim=fail header.d=evil.test; dmarc=fail"
	r := ParseAuthResults(h, "ceo@company.com")
	if r.RiskLevel != "high" {
		t.Fatalf("expected high risk, got %s (score %d, reasons %v)", r.RiskLevel, r.RiskScore, r.Reasons)
	}
	if r.Aligned {
		t.Fatal("expected not aligned")
	}
}

func TestParseAuthResultsMissing(t *testing.T) {
	r := ParseAuthResults("", "someone@somewhere.com")
	if r.RiskScore == 0 {
		t.Fatal("missing header should carry nonzero risk")
	}
}
