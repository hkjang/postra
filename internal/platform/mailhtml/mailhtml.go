// Package mailhtml prepares authored HTML for mail and isolated previews.
// Its allowlist is deliberately independent of the incoming-mail policy:
// authored messages retain safe inline formatting without remote resources.
package mailhtml

import (
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

var policy = newPolicy(false)
var outboundPolicy = newPolicy(true)
var inlineSource = regexp.MustCompile(`^cid:postra-att_[A-Za-z0-9_-]{1,120}@postra\.local$`)

func newPolicy(allowRemote bool) *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("a", "b", "strong", "i", "em", "u", "s", "strike", "del", "sub", "sup",
		"p", "div", "span", "br", "hr", "h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li", "blockquote", "pre", "code",
		"table", "caption", "thead", "tbody", "tfoot", "tr", "th", "td", "img")
	p.AllowAttrs("href", "title").OnElements("a")
	// Renderer-owned signature boundary; contains only an opaque signature ID,
	// never script, style or remote-resource instructions.
	p.AllowAttrs("data-postra-signature").Matching(regexp.MustCompile(`^sig_[A-Za-z0-9_-]{1,120}$`)).OnElements("div")
	p.AllowAttrs("data-postra-template").Matching(regexp.MustCompile(`^(clean|formal|concise|notice|report|newsletter)$`)).OnElements("div")
	p.AllowStandardURLs()
	p.AllowURLSchemes("cid")
	// Only server-owned inline attachment IDs are accepted. Remote images,
	// data URLs, SVG and arbitrary CID references remain forbidden.
	imageURLPattern := `^cid:postra-att_[A-Za-z0-9_-]{1,120}@postra\.local$`
	if allowRemote {
		imageURLPattern = `^(?:cid:postra-att_[A-Za-z0-9_-]{1,120}@postra\.local|https?://[^\s]+)$`
	}
	p.AllowAttrs("src").Matching(regexp.MustCompile(imageURLPattern)).OnElements("img")
	p.AllowAttrs("alt", "title").OnElements("img")
	if allowRemote {
		p.AllowAttrs("data-postra-external-image").Matching(regexp.MustCompile(`^(allow|proxy)$`)).OnElements("img")
	}
	p.RequireNoFollowOnLinks(true)
	p.AllowRelativeURLs(false)
	p.RequireNoFollowOnLinks(true)
	p.AllowAttrs("colspan", "rowspan").Matching(regexp.MustCompile(`^[1-9][0-9]{0,2}$`)).OnElements("td", "th")
	p.AllowAttrs("start").Matching(regexp.MustCompile(`^[0-9]{1,4}$`)).OnElements("ol")
	p.AllowAttrs("align").Matching(regexp.MustCompile(`^(left|center|right)$`)).OnElements("table", "td", "th", "p", "div")
	p.AllowAttrs("valign").Matching(regexp.MustCompile(`^(top|middle|bottom)$`)).OnElements("td", "th")
	p.AllowAttrs("width", "height").Matching(regexp.MustCompile(`^[0-9]{1,4}%?$`)).OnElements("table", "td", "th", "img")
	p.AllowAttrs("cellpadding", "cellspacing", "border").Matching(regexp.MustCompile(`^[0-9]{1,3}$`)).OnElements("table")
	// Use bluemonday's property-specific validators. Do not allow background,
	// images, positioning, display, or arbitrary styles: these could load
	// resources or conceal content that the sender is approving.
	p.AllowStyles("color", "background-color", "font-family", "font-size", "font-weight", "font-style",
		"text-decoration", "text-align", "vertical-align", "line-height", "letter-spacing",
		"padding", "padding-top", "padding-right", "padding-bottom", "padding-left",
		"margin", "margin-top", "margin-right", "margin-bottom", "margin-left",
		"border", "border-top", "border-right", "border-bottom", "border-left",
		"border-width", "border-style", "border-color", "border-radius", "border-collapse", "border-spacing",
		"width", "max-width", "height", "list-style-type", "white-space").Globally()
	return p
}

// Sanitize preserves safe rich text and inline email layouts. Scripts, forms,
// event handlers, unsafe URLs, and externally loaded resources are removed.
func Sanitize(raw string) string {
	safe := strings.TrimSpace(policy.Sanitize(raw))
	return filterImages(safe, false)
}

// SanitizeOutbound retains safe HTTP(S) image references for an already
// authorized outbound payload. Application send gates enforce the live policy.
// This function parses text only and never fetches referenced resources.
func SanitizeOutbound(raw string) string {
	return filterImages(strings.TrimSpace(outboundPolicy.Sanitize(raw)), true)
}

func filterImages(safe string, allowRemote bool) string {
	if !strings.Contains(safe, "<img") {
		return safe
	}
	var out strings.Builder
	tokens := html.NewTokenizer(strings.NewReader(safe))
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		rawToken := string(tokens.Raw())
		keep := true
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			token := tokens.Token()
			if token.Data == "img" {
				keep = false
				for _, attr := range token.Attr {
					if attr.Key == "src" && (inlineSource.MatchString(attr.Val) || allowRemote && validRemoteImage(attr.Val)) {
						keep = true
					}
				}
			}
		}
		if keep {
			out.WriteString(rawToken)
		}
	}
	return out.String()
}

var textWhitespace = regexp.MustCompile(`[\t\r\n\f ]+`)

// PlainText derives the alternative body from sanitized HTML. Inline text is
// kept adjacent (including sensitive values split across spans); paragraphs,
// lists, tables, and links remain understandable in plain-text mail clients.
func PlainText(raw string) string {
	return textContent(raw, false)
}

// PolicyText also includes transmitted attribute values, such as link titles
// and font names. Hiding sensitive content in rich-text metadata must not
// bypass the same outbound policy applied to visible mail text.
func PolicyText(raw string) string {
	return textContent(raw, true)
}

func textContent(raw string, includeAttributes bool) string {
	doc, err := html.Parse(strings.NewReader(SanitizeOutbound(raw)))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var attributes strings.Builder
	newline := func() {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
	}
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, pre bool) {
		if n.Type == html.TextNode {
			if pre {
				b.WriteString(n.Data)
			} else {
				b.WriteString(textWhitespace.ReplaceAllString(n.Data, " "))
			}
			return
		}
		block := false
		if n.Type == html.ElementNode {
			if includeAttributes {
				for _, attr := range n.Attr {
					// Link destinations are already in the plain-text body.
					if attr.Key != "href" {
						attributes.WriteString(attr.Val)
						attributes.WriteByte('\n')
					}
				}
			}
			switch n.Data {
			case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre", "ul", "ol", "li", "tr", "table", "caption":
				block = true
				newline()
			case "br", "hr":
				b.WriteByte('\n')
			}
			if n.Data == "li" {
				b.WriteString("- ")
			}
			if n.Data == "img" {
				for _, attr := range n.Attr {
					if attr.Key == "alt" && attr.Val != "" {
						b.WriteString("[" + attr.Val + "]")
					}
				}
			}
			pre = pre || n.Data == "pre"
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, pre)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a":
				for _, attr := range n.Attr {
					if attr.Key == "href" && attr.Val != "" {
						b.WriteString(" <" + attr.Val + ">")
					}
				}
			case "td", "th":
				b.WriteByte('\t')
			}
		}
		if block {
			newline()
		}
	}
	walk(doc, false)
	lines := strings.Split(b.String(), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if attributes.Len() > 0 {
		text += "\n" + strings.TrimSpace(attributes.String())
	}
	return strings.TrimSpace(text)
}
