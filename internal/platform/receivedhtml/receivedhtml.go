// Package receivedhtml sanitizes untrusted incoming mail separately from
// outbound templates. External image URLs are inert metadata until an
// application-validated, per-user decision explicitly enables rendering.
package receivedhtml

import (
	"bytes"
	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const Marker = "<!--postra-received-html:v1-->"
const imageAttribute = "data-postra-image"

var tinyStyle = regexp.MustCompile(`(?i)(width|height)\s*:\s*[012](?:\.0+)?px\b|display\s*:\s*none|visibility\s*:\s*hidden|opacity\s*:\s*0(?:[;\s]|$)`)
var pixelPath = regexp.MustCompile(`(?i)(?:^|/)(?:pixel|tracking|tracker|beacon|open)(?:[./_-]|$)`)
var policy = buildPolicy()

func buildPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("a", "b", "i", "u", "s", "em", "strong", "p", "br", "hr", "div", "span", "ul", "ol", "li", "blockquote", "pre", "code", "table", "caption", "thead", "tbody", "tfoot", "tr", "td", "th", "h1", "h2", "h3", "h4", "h5", "h6", "img")
	p.AllowAttrs("href", "title").OnElements("a")
	p.AllowStandardURLs()
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AllowAttrs(imageAttribute).Matching(regexp.MustCompile(`^https?://`)).OnElements("img")
	p.AllowAttrs("data-postra-cid").Matching(regexp.MustCompile(`^cid:[A-Za-z0-9@._+\-]{1,200}$`)).OnElements("img")
	p.AllowAttrs("alt", "title").OnElements("img")
	p.AllowAttrs("width", "height").Matching(regexp.MustCompile(`^[0-9]{1,4}%?$`)).OnElements("img", "table", "td", "th")
	p.AllowAttrs("colspan", "rowspan").Matching(regexp.MustCompile(`^[0-9]{1,3}$`)).OnElements("td", "th")
	p.AllowStyles("color", "background-color", "font-family", "font-size", "font-weight", "font-style", "text-decoration", "text-align", "line-height", "padding", "padding-top", "padding-bottom", "padding-left", "padding-right", "margin", "margin-top", "margin-bottom", "margin-left", "margin-right", "border", "border-color", "border-width", "border-style", "border-radius", "border-collapse", "width", "max-width").Globally()
	return p
}

func attr(node *html.Node, name string) string {
	for _, a := range node.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
func remoteURL(raw string) string {
	if len(raw) > 4096 || strings.ContainsAny(raw, "\\\x00\r\n") {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || strings.HasSuffix(strings.ToLower(u.Path), ".svg") || pixelPath.MatchString(u.Path) {
		return ""
	}
	u.Fragment = ""
	return u.String()
}
func tiny(node *html.Node) bool {
	for _, name := range []string{"width", "height"} {
		raw := strings.TrimSuffix(attr(node, name), "px")
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 2 {
			return true
		}
	}
	return tinyStyle.MatchString(attr(node, "style"))
}
func walk(node *html.Node, visit func(*html.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		walk(child, visit)
		child = next
	}
}
func render(node *html.Node) string {
	var out bytes.Buffer
	if html.Render(&out, node) != nil {
		return ""
	}
	return out.String()
}

// Sanitize stores no active external source, srcset, CSS URL or data image.
// CID metadata is retained for future safe attachment resolution, never loaded.
func Sanitize(input string) string {
	doc, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return ""
	}
	walk(doc, func(node *html.Node) {
		if node.Type != html.ElementNode || node.Data != "img" {
			return
		}
		source := attr(node, "src")
		if source == "" {
			source = attr(node, imageAttribute)
		}
		cid := attr(node, "data-postra-cid")
		if strings.HasPrefix(source, "cid:") {
			cid = source
		}
		remote := remoteURL(source)
		if tiny(node) || (remote == "" && cid == "") {
			if node.Parent != nil {
				node.Parent.RemoveChild(node)
			}
			return
		}
		var clean []html.Attribute
		for _, a := range node.Attr {
			if a.Key == "alt" || a.Key == "title" || a.Key == "width" || a.Key == "height" {
				clean = append(clean, a)
			}
		}
		if remote != "" {
			clean = append(clean, html.Attribute{Key: imageAttribute, Val: remote})
		} else {
			clean = append(clean, html.Attribute{Key: "data-postra-cid", Val: cid})
		}
		node.Attr = clean
	})
	return Marker + policy.Sanitize(render(doc))
}

// Render re-sanitizes legacy/imported bodies and adds HTTP(S) src only after
// the application has enforced the administrator policy and user's choice.
func Render(input string, allow bool) (output string, external int) {
	doc, err := html.Parse(strings.NewReader(Sanitize(input)))
	if err != nil {
		return "", 0
	}
	walk(doc, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "img" {
			if source := remoteURL(attr(node, imageAttribute)); source != "" {
				external++
				if allow {
					node.Attr = append(node.Attr, html.Attribute{Key: "src", Val: source})
				}
			}
		}
	})
	return render(doc), external
}
