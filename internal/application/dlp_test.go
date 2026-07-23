package application

import (
	"testing"

	"postra/internal/platform/config"
)

func TestScanDLPDetectsSensitiveShapesAndKeywords(t *testing.T) {
	a := &App{Cfg: config.Config{Send: config.SendConfig{DLPKeywords: []string{"대외비", "confidential"}}}}
	findings := a.scanDLP("Q3 plan (CONFIDENTIAL)", "고객 주민번호 900101-1234567 첨부합니다. 대외비 문서.")
	types := map[string]bool{}
	for _, f := range findings {
		if f.Type == "KEYWORD" {
			types["kw:"+f.Term] = true
		} else {
			types[f.Type] = true
		}
	}
	if !types["RRN"] {
		t.Errorf("expected RRN finding, got %+v", findings)
	}
	if !types["kw:대외비"] || !types["kw:confidential"] {
		t.Errorf("expected keyword findings, got %+v", findings)
	}
}

func TestDLPPolicyDefault(t *testing.T) {
	a := &App{Cfg: config.Config{Send: config.SendConfig{DLPPolicy: ""}}}
	if a.dlpPolicy() != "warn" {
		t.Fatalf("default policy should be warn, got %q", a.dlpPolicy())
	}
	a.Cfg.Send.DLPPolicy = "BLOCK"
	if a.dlpPolicy() != "block" {
		t.Fatalf("policy should normalize to block, got %q", a.dlpPolicy())
	}
}

func TestScanDLPIgnoresUbiquitousEmailPhone(t *testing.T) {
	a := &App{Cfg: config.Config{}}
	findings := a.scanDLP("hello", "reach me at bob@example.com or +1 555 123 4567")
	for _, f := range findings {
		if f.Type == "EMAIL" || f.Type == "PHONE" {
			t.Fatalf("email/phone should not be DLP findings: %+v", findings)
		}
	}
}
