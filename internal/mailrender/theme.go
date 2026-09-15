package mailrender

import (
	"sort"
	"strings"

	"github.com/aymerick/douceur/parser"
	"golang.org/x/net/html"
)

type Template struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func Templates() []Template {
	return []Template{
		{"clean", "기본", "읽기 편한 여백과 파란 강조"},
		{"formal", "격식", "공식 문서에 어울리는 절제된 서식"},
		{"concise", "간결", "짧은 회신을 위한 촘촘한 간격"},
		{"notice", "공지", "중요 안내를 강조하는 테두리"},
		{"report", "업무 보고", "표와 목록이 명확한 업무 공유"},
		{"newsletter", "뉴스레터", "넓은 제목과 편안한 읽기 너비"},
		{"plain", "일반 텍스트", "HTML 없이 텍스트로 발송"},
	}
}

func ValidTemplate(id string) bool {
	for _, t := range Templates() {
		if t.ID == id {
			return true
		}
	}
	return false
}

func themeHTML(fragment, name string) string {
	font, size := "Arial, sans-serif", "15px"
	box := ""
	switch name {
	case "formal":
		font = "Georgia, serif"
	case "concise":
		size = "14px"
	case "notice":
		box = "border-left:4px solid #e5b454;background-color:#fffbef;"
	case "report":
		box = "border:1px solid #dfe5ee;"
	}
	styles := themeElementStyles(name)
	// Styling is attached to elements, not a stylesheet: mail clients routinely
	// discard <head>/<style>. Existing allowed author styles take precedence.
	doc, err := html.Parse(strings.NewReader(fragment))
	if err != nil {
		return fragment
	}
	var body *html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			if node.Data == "body" {
				body = node
			}
			style := styles[node.Data]
			if style != "" {
				found := false
				for i := range node.Attr {
					if node.Attr[i].Key == "style" {
						node.Attr[i].Val = mergeStyles(style, node.Attr[i].Val)
						found = true
						break
					}
				}
				if !found {
					node.Attr = append(node.Attr, html.Attribute{Key: "style", Val: mergeStyles(style, "")})
				}
			}
		}
		for child := node.FirstChild; child != nil; {
			next := child.NextSibling
			isWrapper := false
			previousTheme := ""
			for _, attr := range child.Attr {
				if child.Type == html.ElementNode && child.Data == "div" && attr.Key == "data-postra-template" {
					isWrapper = true
					previousTheme = attr.Val
				}
			}
			if isWrapper {
				previousStyles := themeElementStyles(previousTheme)
				for child.FirstChild != nil {
					inner := child.FirstChild
					child.RemoveChild(inner)
					node.InsertBefore(inner, child)
					removeThemeDefaults(inner, previousStyles)
					walk(inner)
				}
				node.RemoveChild(child)
			} else {
				walk(child)
			}
			child = next
		}
	}
	walk(doc)
	var content strings.Builder
	if body != nil {
		for child := body.FirstChild; child != nil; child = child.NextSibling {
			_ = html.Render(&content, child)
		}
	}
	return `<div data-postra-template="` + name + `" style="font-family:` + font + `;font-size:` + size + `;line-height:1.7;color:#172033;max-width:680px;margin:0 auto;padding:24px;` + box + `">` + content.String() + `</div>`
}

func themeElementStyles(name string) map[string]string {
	accent, spacing := "#3157d5", "16px"
	switch name {
	case "formal":
		accent = "#334155"
	case "concise":
		spacing = "8px"
	case "notice":
		accent = "#9a6700"
	case "report":
		accent = "#245699"
	case "newsletter":
		accent, spacing = "#7550b6", "22px"
	}
	styles := map[string]string{
		"p":          "margin:0 0 " + spacing + ";line-height:1.7;",
		"h1":         "font-size:28px;line-height:1.35;margin:0 0 " + spacing + ";color:" + accent + ";",
		"h2":         "font-size:23px;line-height:1.4;margin:20px 0 " + spacing + ";color:" + accent + ";",
		"h3":         "font-size:18px;line-height:1.4;margin:18px 0 12px;color:" + accent + ";",
		"ul":         "margin:0 0 " + spacing + ";padding-left:24px;",
		"li":         "margin-bottom:6px;line-height:1.7;",
		"a":          "color:" + accent + ";text-decoration:underline;",
		"blockquote": "border-left:3px solid #c6d2eb;padding-left:16px;margin:16px 0;color:#596579;",
		"table":      "border-collapse:collapse;width:100%;margin:16px 0;",
		"th":         "border:1px solid #dfe5ee;padding:12px;text-align:left;background-color:#eef2f8;",
		"td":         "border:1px solid #dfe5ee;padding:12px;vertical-align:top;",
		"pre":        "padding:16px;background-color:#f4f6fa;white-space:pre-wrap;font-family:monospace;",
		"code":       "font-family:monospace;",
		"hr":         "border:0;border-top:1px solid #dfe5ee;margin:20px 0;",
	}
	styles["h4"], styles["h5"], styles["h6"], styles["ol"] = styles["h3"], styles["h3"], styles["h3"], styles["ul"]
	return styles
}

func removeThemeDefaults(node *html.Node, styles map[string]string) {
	defaults, _ := parser.ParseDeclarations(styles[node.Data])
	values := map[string]string{}
	for _, item := range defaults {
		values[item.Property] = item.Value
	}
	for i, attr := range node.Attr {
		if attr.Key != "style" {
			continue
		}
		declarations, _ := parser.ParseDeclarations(strings.TrimRight(strings.TrimSpace(attr.Val), ";") + ";")
		var retained strings.Builder
		for _, declaration := range declarations {
			if expected, ok := values[declaration.Property]; !ok || expected != declaration.Value {
				retained.WriteString(declaration.Property + ":" + declaration.Value + ";")
			}
		}
		node.Attr[i].Val = retained.String()
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		removeThemeDefaults(child, styles)
	}
}

func mergeStyles(defaults, authored string) string {
	values := map[string]string{}
	for _, raw := range []string{defaults, authored} {
		// The CSS parser expects the final semicolon; the sanitizer intentionally
		// serializes declarations without one. Normalize before merging, otherwise
		// every subsequent edit would silently lose the last authored declaration.
		declarations, err := parser.ParseDeclarations(strings.TrimRight(strings.TrimSpace(raw), ";") + ";")
		if err != nil {
			continue
		}
		for _, declaration := range declarations {
			values[strings.ToLower(declaration.Property)] = declaration.Value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		out.WriteString(key + ":" + values[key] + ";")
	}
	return out.String()
}
