package imap

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"
	"unicode/utf16"

	"postra/internal/domain"
)

// reListLine splits a LIST response into its attributes, hierarchy delimiter
// and mailbox name: `* LIST (\HasNoChildren \Sent) "/" "Sent"`. The name may
// be quoted or an atom; a literal name ({n}) is not matched and is skipped.
var reListLine = regexp.MustCompile(`^\* LIST \(([^)]*)\) (NIL|"(?:[^"\\]|\\.)*") (.+)$`)

// sentNames are the Sent folder names servers use when they do not report the
// RFC 6154 \Sent attribute. They are compared, lower-cased, against the last
// hierarchy segment of the decoded name, so "INBOX.Sent" and
// "[Gmail]/Sent Mail" match too.
var sentNames = map[string]bool{
	"sent": true, "sent items": true, "sent messages": true, "sent mail": true,
	"보낸편지함": true, "보낸 편지함": true, "보낸메일함": true, "보낸 메일함": true, "보낸메일": true,
}

// SentMailbox names the account's sent-mail folder, or "" when the server has
// none. The \Sent attribute wins — Gmail, Exchange and Dovecot with
// SPECIAL-USE report it on a plain LIST — then a well-known name. Names are
// decoded from modified UTF-7 for matching and returned as the server spelled
// them, ready for SELECT.
func (s *session) SentMailbox(ctx context.Context) (string, error) {
	lines, err := s.exec(`LIST "" "*"`)
	if err != nil {
		return "", err
	}
	byName := ""
	for _, line := range lines {
		m := reListLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		attrs, delim, raw := m[1], unquoteIMAP(m[2]), unquoteIMAP(strings.TrimSpace(m[3]))
		if raw == "" || containsAttr(attrs, `\Noselect`) || containsAttr(attrs, `\NonExistent`) {
			continue
		}
		if containsAttr(attrs, `\Sent`) {
			return raw, nil
		}
		if byName == "" {
			name := decodeModifiedUTF7(raw)
			if delim != "" && delim != "NIL" {
				if i := strings.LastIndex(name, delim); i >= 0 {
					name = name[i+len(delim):]
				}
			}
			if sentNames[strings.ToLower(strings.TrimSpace(name))] {
				byName = raw
			}
		}
	}
	return byName, nil
}

var _ domain.SentFolderCapable = (*session)(nil)

func containsAttr(attrs, want string) bool {
	for _, a := range strings.Fields(attrs) {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

// unquoteIMAP undoes an IMAP quoted string; an atom is returned unchanged.
func unquoteIMAP(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	var b strings.Builder
	escaped := false
	for _, r := range s[1 : len(s)-1] {
		if escaped || r != '\\' {
			b.WriteRune(r)
			escaped = false
			continue
		}
		escaped = true
	}
	return b.String()
}

// decodeModifiedUTF7 decodes an RFC 3501 §5.1.3 mailbox name: "&" opens a
// run of modified base64 (',' for '/') over UTF-16BE, closed by "-", and "&-"
// is a literal "&". An undecodable run is kept as written.
func decodeModifiedUTF7(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}
		end := strings.IndexByte(s[i+1:], '-')
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		run := s[i+1 : i+1+end]
		i += end + 1
		if run == "" {
			b.WriteByte('&')
			continue
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.ReplaceAll(run, ",", "/"))
		if err != nil || len(raw)%2 != 0 {
			b.WriteString("&" + run + "-")
			continue
		}
		units := make([]uint16, len(raw)/2)
		for j := range units {
			units[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}
		b.WriteString(string(utf16.Decode(units)))
	}
	return b.String()
}
