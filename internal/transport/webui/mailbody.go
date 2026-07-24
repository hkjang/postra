package webui

import (
	"html"
	"html/template"
	"strconv"
	"strings"

	"regexp"
)

var (
	// One pass matches URLs and bare emails so inserted <a> markup is never
	// re-scanned (which would nest tags). Applied to already-escaped text.
	reMailLink = regexp.MustCompile(`https?://[^\s<>"']+|[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reMailBold = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reMailCode = regexp.MustCompile("`([^`\n]+)`")
	reMailHead = regexp.MustCompile(`^(#{1,3})\s+(.+)$`)
)

// renderMailHTML returns an email's server-sanitized HTML body for display.
// The bytes were already run through the strict bluemonday policy at ingest
// (scripts, event handlers, style, and external resources removed; only
// cid:/data:image survive) — that sanitization is the security boundary, so
// re-doing it here would be redundant. Because style attributes are stripped,
// the .mail-body stylesheet fully controls the appearance for a clean, uniform
// look regardless of the sender's original markup.
func renderMailHTML(sanitized string) template.HTML {
	return template.HTML(sanitized) // #nosec G203 -- pre-sanitized at ingest by bluemonday
}

// renderMailText turns a plaintext email body into tasteful, safe HTML: it
// escapes everything first, then autolinks URLs/emails, renders quoted (">")
// runs as blockquotes, and applies light, unambiguous markdown (h1–h3,
// **bold**, `code`) — deliberately avoiding the aggressive rules (single *,
// underscores, setext) that would mangle ordinary email text.
func renderMailText(text string) template.HTML {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	var out, para, quote strings.Builder
	flushPara := func() {
		if para.Len() > 0 {
			out.WriteString("<p>" + para.String() + "</p>")
			para.Reset()
		}
	}
	flushQuote := func() {
		if quote.Len() > 0 {
			out.WriteString("<blockquote>" + quote.String() + "</blockquote>")
			quote.Reset()
		}
	}
	appendBR := func(b *strings.Builder, s string) {
		if b.Len() > 0 {
			b.WriteString("<br>")
		}
		b.WriteString(s)
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case strings.HasPrefix(trimmed, ">"): // quoted reply
			flushPara()
			content := strings.TrimPrefix(trimmed, ">")
			appendBR(&quote, inlineMail(strings.TrimSpace(strings.TrimLeft(content, ">"))))
		case strings.TrimSpace(line) == "": // blank line ends a paragraph
			flushQuote()
			flushPara()
		default:
			flushQuote()
			if m := reMailHead.FindStringSubmatch(line); m != nil {
				flushPara()
				lvl := len(m[1]) + 2 // #→h3 … ###→h5
				if lvl > 5 {
					lvl = 5
				}
				tag := "h" + strconv.Itoa(lvl)
				out.WriteString("<" + tag + " class=\"mb-h\">" + inlineMail(m[2]) + "</" + tag + ">")
				continue
			}
			appendBR(&para, inlineMail(line))
		}
	}
	flushQuote()
	flushPara()
	return template.HTML(out.String()) // #nosec G203 -- input escaped in inlineMail; only safe tags inserted
}

// inlineMail escapes one line and applies safe inline formatting.
func inlineMail(s string) string {
	s = html.EscapeString(s)
	s = reMailLink.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "http") {
			// Peel trailing punctuation so "see http://x)." links only the URL.
			trail := ""
			for len(m) > 0 && strings.ContainsRune(".,;:!?)]}", rune(m[len(m)-1])) {
				trail = string(m[len(m)-1]) + trail
				m = m[:len(m)-1]
			}
			return `<a href="` + m + `" target="_blank" rel="noopener noreferrer nofollow">` + m + `</a>` + trail
		}
		return `<a href="mailto:` + m + `">` + m + `</a>`
	})
	s = reMailBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = reMailCode.ReplaceAllString(s, "<code>$1</code>")
	return s
}
