package mailrender

import (
	"golang.org/x/net/html"
	"postra/internal/platform/mailhtml"
	"strings"
)

func RemoveInlineImage(raw, contentID string) string {
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
			if child.Type == html.ElementNode && child.Data == "img" {
				for _, attr := range child.Attr {
					if attr.Key == "src" && attr.Val == "cid:"+contentID {
						remove = true
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
		for node := body.FirstChild; node != nil; node = node.NextSibling {
			_ = html.Render(&out, node)
		}
	}
	return out.String()
}

// InlineImages returns sanitized attachment references, never remote assets.
func InlineImages(raw string) (markup string, ids []string) {
	tokens := html.NewTokenizer(strings.NewReader(mailhtml.Sanitize(raw)))
	var out strings.Builder
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := tokens.Token()
		if token.Data != "img" {
			continue
		}
		for _, attribute := range token.Attr {
			if attribute.Key == "src" && strings.HasPrefix(attribute.Val, "cid:") {
				ids = append(ids, strings.TrimPrefix(attribute.Val, "cid:"))
				out.WriteString(token.String())
			}
		}
	}
	return out.String(), ids
}
