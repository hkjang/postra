package mailrender

import (
	"strings"

	"golang.org/x/net/html"

	"postra/internal/platform/mailhtml"
)

// StripSignatureHTML removes only explicitly marked signature blocks. It never
// guesses based on a person's name or on arbitrary text already in the mail.
func StripSignatureHTML(raw string) string {
	doc, err := html.Parse(strings.NewReader(mailhtml.SanitizeOutbound(raw)))
	if err != nil {
		return mailhtml.SanitizeOutbound(raw)
	}
	var body *html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "body" {
			body = node
		}
		for child := node.FirstChild; child != nil; {
			next := child.NextSibling
			remove := false
			if child.Type == html.ElementNode && child.Data == "div" {
				for _, attr := range child.Attr {
					if attr.Key == "data-postra-signature" {
						remove = true
						break
					}
				}
			}
			if remove {
				node.RemoveChild(child)
			} else {
				walk(child)
			}
			child = next
		}
	}
	walk(doc)
	var out strings.Builder
	if body != nil {
		for child := body.FirstChild; child != nil; child = child.NextSibling {
			_ = html.Render(&out, child)
		}
	}
	return out.String()
}
