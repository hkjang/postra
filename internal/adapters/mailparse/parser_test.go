package mailparse

import (
	"strings"
	"testing"
)

const multipartMail = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: =?utf-8?B?7ZWc6riAIOygnOuqqQ==?=\r\n" +
	"Date: Mon, 13 Jul 2026 10:00:00 +0900\r\n" +
	"Message-ID: <m1@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"B1\"\r\n" +
	"\r\n" +
	"--B1\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"hello body\r\n" +
	"--B1\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p onclick=\"evil()\">hi</p><script>alert(1)</script><img src=\"http://evil/x.png\"><img src=\"cid:inline1\">\r\n" +
	"--B1\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"../../etc/passwd\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"aGVsbG8=\r\n" +
	"--B1--\r\n"

func TestParseMultipart(t *testing.T) {
	p := Parse([]byte(multipartMail))
	if p.ParseError != "" {
		t.Fatalf("unexpected parse error: %s", p.ParseError)
	}
	if p.Subject != "한글 제목" {
		t.Errorf("subject decode failed: %q", p.Subject)
	}
	if p.From.Email != "alice@example.com" {
		t.Errorf("from: %+v", p.From)
	}
	if !strings.Contains(p.TextBody, "hello body") {
		t.Errorf("text body: %q", p.TextBody)
	}
	// MIME-008: scripts and event handlers removed
	if strings.Contains(p.HTMLSafe, "script") || strings.Contains(p.HTMLSafe, "onclick") {
		t.Errorf("sanitizer left dangerous content: %q", p.HTMLSafe)
	}
	// MIME-009: external image blocked, cid kept
	if strings.Contains(p.HTMLSafe, "http://evil") {
		t.Errorf("external image survived: %q", p.HTMLSafe)
	}
	if !strings.Contains(p.HTMLSafe, "cid:inline1") {
		t.Errorf("cid image should survive: %q", p.HTMLSafe)
	}
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments: %d", len(p.Attachments))
	}
	// MIME-010: traversal removed
	if strings.Contains(p.Attachments[0].Name, "..") || strings.Contains(p.Attachments[0].Name, "/") {
		t.Errorf("unsafe filename: %q", p.Attachments[0].Name)
	}
	if string(p.Attachments[0].Data) != "hello" {
		t.Errorf("base64 decode: %q", p.Attachments[0].Data)
	}
}

func TestParseBrokenMIME(t *testing.T) {
	// MIME-004: malformed input must not panic and must keep something.
	p := Parse([]byte("garbage without headers"))
	if p.TextBody == "" && p.ParseError == "" {
		t.Fatal("broken mail should record a parse error or fallback body")
	}
}

func TestSubjectKey(t *testing.T) {
	for _, tc := range [][2]string{
		{"Re: Re: Hello World", "hello world"},
		{"FWD: 회의 일정", "회의 일정"},
		{"답장: RE: test", "test"},
	} {
		if got := SubjectKey(tc[0]); got != tc[1] {
			t.Errorf("SubjectKey(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

func TestQuotedPrintableAndCharset(t *testing.T) {
	mail := "From: a@b.c\r\nSubject: t\r\n" +
		"Content-Type: text/plain; charset=euc-kr\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"vsiz58fPvLy/5A==\r\n" // "안녕하세요" in EUC-KR
	p := Parse([]byte(mail))
	if !strings.Contains(p.TextBody, "안녕하세요") {
		t.Errorf("EUC-KR decode failed: %q", p.TextBody)
	}
}

func TestReferenceIDs(t *testing.T) {
	refs := ReferenceIDs("<a@x> <b@x>", "<b@x>")
	if len(refs) != 2 {
		t.Fatalf("want 2 unique refs, got %v", refs)
	}
}
