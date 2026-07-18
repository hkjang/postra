package mailparse

import "testing"

// FuzzParse ensures the MIME parser never panics on malformed input
// (§20.3 Fuzz Test requirement for MIME/Header parsing).
func FuzzParse(f *testing.F) {
	f.Add([]byte("From: a@b.c\r\nSubject: hi\r\n\r\nbody"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=X\r\n\r\n--X\r\n\r\nz\r\n--X--"))
	f.Add([]byte("garbage"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		p := Parse(data) // must not panic
		_ = p.Subject
		_ = SubjectKey(p.Subject)
		_ = ReferenceIDs(p.References, p.InReplyTo)
	})
}

func FuzzSanitizeFilename(f *testing.F) {
	f.Add("../../etc/passwd")
	f.Add("normal.pdf")
	f.Fuzz(func(t *testing.T, name string) {
		got := SanitizeFilename(name)
		if got == "" {
			t.Fatal("sanitized filename must never be empty")
		}
		for _, r := range got {
			if r == '/' || r == '\\' || r == 0 {
				t.Fatalf("sanitized filename retains unsafe rune: %q", got)
			}
		}
	})
}
