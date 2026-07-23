// Package mailparse decodes RFC822/MIME messages into normalized parts:
// headers, text body, sanitized HTML, and attachments. Parsing is
// best-effort — a malformed part records a parse error instead of failing
// the whole message (MIME-004).
package mailparse

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html/charset"

	"postra/internal/domain"
)


type AttachmentPart struct {
	Name     string
	MIMEType string
	Inline   bool
	Data     []byte
}

type Parsed struct {
	Subject     string
	From        domain.Address
	To          []domain.Address
	Cc          []domain.Address
	ReplyTo     []domain.Address
	Date        time.Time
	MessageID   string
	InReplyTo   string
	References  string
	AuthResults string
	TextBody    string
	HTMLSafe    string
	Charset     string
	Attachments []AttachmentPart
	ParseError  string
}

const (
	maxParts      = 200
	maxPartDepth  = 10
	maxDecodedLen = 100 << 20
)

var wordDecoder = &mime.WordDecoder{
	CharsetReader: func(label string, input io.Reader) (io.Reader, error) {
		return charset.NewReaderLabel(label, input)
	},
}

// htmlPolicy strips scripts/event handlers and blocks external resources:
// only cid:/data: image sources survive (MIME-008/009).
var htmlPolicy = buildPolicy()

func buildPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("a", "b", "i", "u", "em", "strong", "p", "br", "hr", "div", "span",
		"ul", "ol", "li", "blockquote", "pre", "code",
		"table", "thead", "tbody", "tr", "td", "th",
		"h1", "h2", "h3", "h4", "h5", "h6", "img", "font")
	p.AllowAttrs("href").OnElements("a")
	p.AllowStandardURLs()
	p.RequireNoFollowOnLinks(true)
	// Only embedded images survive: cid: references and data: image URIs.
	// External http(s) sources are dropped so nothing auto-loads (MIME-009).
	p.AllowURLSchemeWithCustomPolicy("cid", func(*url.URL) bool { return true })
	p.AllowURLSchemeWithCustomPolicy("data", func(u *url.URL) bool {
		return strings.HasPrefix(u.Opaque, "image/")
	})
	p.AllowAttrs("src").Matching(regexp.MustCompile(`^(cid:|data:image/)`)).OnElements("img")
	p.AllowAttrs("alt", "width", "height").OnElements("img")
	return p
}

func Parse(raw []byte) *Parsed {
	out := &Parsed{}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		out.ParseError = "header parse: " + err.Error()
		out.TextBody = string(raw) // preserve something searchable
		return out
	}

	h := msg.Header
	out.Subject = decodeWord(h.Get("Subject"))
	out.MessageID = strings.TrimSpace(h.Get("Message-ID"))
	out.InReplyTo = strings.TrimSpace(h.Get("In-Reply-To"))
	out.References = strings.TrimSpace(h.Get("References"))
	out.AuthResults = summarizeAuthResults(h.Get("Authentication-Results"))
	if t, err := mail.ParseDate(h.Get("Date")); err == nil {
		out.Date = t
	}
	out.From = firstAddr(parseAddrs(h.Get("From")))
	out.To = parseAddrs(h.Get("To"))
	out.Cc = parseAddrs(h.Get("Cc"))
	out.ReplyTo = parseAddrs(h.Get("Reply-To"))

	partCount := 0
	if err := walkPart(msg.Body, h.Get("Content-Type"), h.Get("Content-Transfer-Encoding"), h.Get("Content-Disposition"), out, 0, &partCount); err != nil {
		out.ParseError = joinErr(out.ParseError, err.Error())
	}
	return out
}

func walkPart(r io.Reader, contentType, cte, disposition string, out *Parsed, depth int, count *int) error {
	if depth > maxPartDepth {
		return fmt.Errorf("multipart nesting exceeds %d", maxPartDepth)
	}
	*count++
	if *count > maxParts {
		return fmt.Errorf("too many MIME parts (>%d)", maxParts)
	}
	if contentType == "" {
		contentType = "text/plain; charset=us-ascii"
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType, params = "text/plain", map[string]string{}
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart without boundary")
		}
		mr := multipart.NewReader(r, boundary)
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("multipart read: %w", err)
			}
			perr := walkPart(p,
				p.Header.Get("Content-Type"),
				p.Header.Get("Content-Transfer-Encoding"),
				p.Header.Get("Content-Disposition"), out, depth+1, count)
			if perr != nil {
				out.ParseError = joinErr(out.ParseError, perr.Error())
			}
		}
	}

	body, err := decodeBody(r, cte)
	if err != nil {
		return fmt.Errorf("decode %s: %w", mediaType, err)
	}

	dispType := ""
	dispParams := map[string]string{}
	if disposition != "" {
		dispType, dispParams, _ = mime.ParseMediaType(disposition)
	}
	isAttachment := dispType == "attachment" ||
		(dispType == "inline" && dispParams["filename"] != "") ||
		(!strings.HasPrefix(mediaType, "text/") && !strings.HasPrefix(mediaType, "multipart/"))

	if isAttachment {
		name := dispParams["filename"]
		if name == "" {
			name = params["name"]
		}
		out.Attachments = append(out.Attachments, AttachmentPart{
			Name:     SanitizeFilename(decodeWord(name)),
			MIMEType: mediaType,
			Inline:   dispType == "inline",
			Data:     body,
		})
		return nil
	}

	text := decodeCharset(body, params["charset"])
	switch mediaType {
	case "text/plain":
		if out.TextBody == "" {
			out.TextBody = text
			out.Charset = params["charset"]
		}
	case "text/html":
		if out.HTMLSafe == "" {
			out.HTMLSafe = htmlPolicy.Sanitize(text)
			if out.TextBody == "" {
				out.TextBody = htmlToText(text)
			}
		}
	default:
		if out.TextBody == "" {
			out.TextBody = text
		}
	}
	return nil
}

func decodeBody(r io.Reader, cte string) ([]byte, error) {
	lr := io.LimitReader(r, maxDecodedLen)
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return io.ReadAll(base64.NewDecoder(base64.StdEncoding,
			&whitespaceFilter{r: lr}))
	case "quoted-printable":
		b, err := io.ReadAll(quotedprintable.NewReader(lr))
		if err != nil && len(b) > 0 {
			return b, nil // keep partial output on soft QP errors
		}
		return b, err
	default:
		return io.ReadAll(lr)
	}
}

// whitespaceFilter strips CR/LF so base64 bodies with folded lines decode.
type whitespaceFilter struct{ r io.Reader }

func (w *whitespaceFilter) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	out := 0
	for i := 0; i < n; i++ {
		if p[i] == '\r' || p[i] == '\n' {
			continue
		}
		p[out] = p[i]
		out++
	}
	return out, err
}

func decodeCharset(b []byte, label string) string {
	if label == "" || strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "us-ascii") {
		return string(b)
	}
	r, err := charset.NewReaderLabel(label, bytes.NewReader(b))
	if err != nil {
		return string(b)
	}
	dec, err := io.ReadAll(r)
	if err != nil {
		return string(b)
	}
	return string(dec)
}


func decodeWord(s string) string {
	d, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return s
	}
	return d
}

func parseAddrs(v string) []domain.Address {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	list, err := mail.ParseAddressList(v)
	if err != nil {
		return []domain.Address{{Email: strings.TrimSpace(v)}}
	}
	out := make([]domain.Address, 0, len(list))
	for _, a := range list {
		out = append(out, domain.Address{Name: decodeWord(a.Name), Email: a.Address})
	}
	return out
}

func firstAddr(a []domain.Address) domain.Address {
	if len(a) == 0 {
		return domain.Address{}
	}
	return a[0]
}

var tagRe = regexp.MustCompile(`(?s)<style.*?</style>|<script.*?</script>|<[^>]*>`)
var spaceRe = regexp.MustCompile(`[ \t]{2,}`)

func htmlToText(html string) string {
	t := tagRe.ReplaceAllString(html, " ")
	t = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(t)
	return strings.TrimSpace(spaceRe.ReplaceAllString(t, " "))
}

var unsafeFile = regexp.MustCompile(`[\x00-\x1f/\\:*?"<>|]`)

// SanitizeFilename removes path traversal and control characters (MIME-010).
func SanitizeFilename(name string) string {
	name = unsafeFile.ReplaceAllString(name, "_")
	name = strings.Trim(name, ". ")
	for strings.Contains(name, "..") {
		name = strings.ReplaceAll(name, "..", "_")
	}
	if name == "" {
		name = "attachment"
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}

var reSubjectPrefix = regexp.MustCompile(`(?i)^\s*((re|fw|fwd|답장|전달)\s*(\[\d+\])?\s*:\s*)+`)

// SubjectKey normalizes a subject for fallback threading (MIME-007).
func SubjectKey(subject string) string {
	s := reSubjectPrefix.ReplaceAllString(subject, "")
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// ReferenceIDs extracts message-ids from References + In-Reply-To.
func ReferenceIDs(references, inReplyTo string) []string {
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.Fields(references + " " + inReplyTo) {
		tok = strings.TrimSpace(tok)
		if strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">") && !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	return out
}

func summarizeAuthResults(v string) string {
	if v == "" {
		return ""
	}
	var parts []string
	for _, key := range []string{"spf", "dkim", "dmarc"} {
		re := regexp.MustCompile(`(?i)\b` + key + `\s*=\s*(\w+)`)
		if m := re.FindStringSubmatch(v); m != nil {
			parts = append(parts, key+"="+strings.ToLower(m[1]))
		}
	}
	return strings.Join(parts, " ")
}

func joinErr(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
