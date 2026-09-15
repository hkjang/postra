package mailrender

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var headingLine = regexp.MustCompile(`^ {0,3}(#{1,6})\s+(.+?)\s*#*\s*$`)
var listLine = regexp.MustCompile(`^ {0,3}([-*+]|[0-9]+[.)])\s+(.*)$`)
var tableSeparator = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$`)

// markdownHTML supports mail-oriented CommonMark constructs and pipe tables.
// Raw HTML is text in Markdown; only explicit HTML mode can provide markup.
// Images are reduced to alt text because authored mail never loads resources.
func markdownHTML(body string) string {
	lines := strings.Split(body, "\n")
	var result strings.Builder
	for i := 0; i < len(lines); {
		line := lines[i]
		trim := strings.TrimSpace(line)
		if trim == "" {
			i++
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			fence := trim[:3]
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++
			}
			result.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>")
			continue
		}
		if match := headingLine.FindStringSubmatch(line); match != nil {
			level := strconv.Itoa(len(match[1]))
			result.WriteString("<h" + level + ">" + inlineMarkdown(match[2], 0) + "</h" + level + ">")
			i++
			continue
		}
		if i+1 < len(lines) && strings.Contains(line, "|") && tableSeparator.MatchString(lines[i+1]) {
			headers := tableCells(line)
			result.WriteString("<table><thead><tr>")
			for _, cell := range headers {
				result.WriteString("<th>" + inlineMarkdown(cell, 0) + "</th>")
			}
			result.WriteString("</tr></thead><tbody>")
			i += 2
			for i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
				cells := tableCells(lines[i])
				result.WriteString("<tr>")
				for j := range headers {
					cell := ""
					if j < len(cells) {
						cell = cells[j]
					}
					result.WriteString("<td>" + inlineMarkdown(cell, 0) + "</td>")
				}
				result.WriteString("</tr>")
				i++
			}
			result.WriteString("</tbody></table>")
			continue
		}
		if listLine.MatchString(line) {
			first := listLine.FindStringSubmatch(line)
			ordered := len(first[1]) > 1
			tag := "ul"
			if ordered {
				tag = "ol"
			}
			result.WriteString("<" + tag + ">")
			for i < len(lines) {
				match := listLine.FindStringSubmatch(lines[i])
				if match == nil || (len(match[1]) > 1) != ordered {
					break
				}
				result.WriteString("<li>" + inlineMarkdown(match[2], 0) + "</li>")
				i++
			}
			result.WriteString("</" + tag + ">")
			continue
		}
		if strings.HasPrefix(trim, ">") {
			var quotes []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				quotes = append(quotes, strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">"), " "))
				i++
			}
			result.WriteString("<blockquote>" + textInlineParagraph(strings.Join(quotes, "\n")) + "</blockquote>")
			continue
		}
		if trim == "---" || trim == "***" || trim == "___" {
			result.WriteString("<hr>")
			i++
			continue
		}
		var paragraph []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			if len(paragraph) > 0 && (headingLine.MatchString(lines[i]) || listLine.MatchString(lines[i]) || strings.HasPrefix(strings.TrimSpace(lines[i]), ">") || strings.HasPrefix(strings.TrimSpace(lines[i]), "```") || (i+1 < len(lines) && tableSeparator.MatchString(lines[i+1]))) {
				break
			}
			paragraph = append(paragraph, lines[i])
			i++
		}
		result.WriteString(textInlineParagraph(strings.Join(paragraph, "\n")))
	}
	return result.String()
}

func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func textInlineParagraph(text string) string {
	return "<p>" + strings.ReplaceAll(inlineMarkdown(text, 0), "\n", "<br>") + "</p>"
}

func safeLink(value string) bool {
	if strings.ContainsAny(value, "\r\n\t\x00 ") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "mailto"
}

func inlineMarkdown(text string, depth int) string {
	if depth > 8 {
		return html.EscapeString(text)
	}
	var out strings.Builder
	for len(text) > 0 {
		if strings.HasPrefix(text, "\\") && len(text) > 1 && strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", rune(text[1])) {
			out.WriteString(html.EscapeString(text[1:2]))
			text = text[2:]
			continue
		}
		image := strings.HasPrefix(text, "![")
		if strings.HasPrefix(text, "[") || image {
			start := 1
			if image {
				start = 2
			}
			end := strings.Index(text[start:], "](")
			if end >= 0 {
				end += start
				tail := text[end+2:]
				if close := strings.IndexByte(tail, ')'); close >= 0 {
					label := text[start:end]
					target := strings.TrimSpace(tail[:close])
					if !image && safeLink(target) {
						out.WriteString(`<a href="` + html.EscapeString(target) + `">` + inlineMarkdown(label, depth+1) + `</a>`)
					} else {
						out.WriteString(inlineMarkdown(label, depth+1))
					}
					text = tail[close+1:]
					continue
				}
				// No later link can close either; avoid rescanning malformed
				// megabyte-sized bracket strings quadratically.
				out.WriteString(html.EscapeString(text))
				break
			}
			// Avoid quadratic rescanning of a long, unmatched bracket sequence.
			if end < 0 {
				out.WriteString(html.EscapeString(text))
				break
			}
		}
		matched := false
		for _, mark := range []struct{ token, tag string }{{"**", "strong"}, {"__", "strong"}, {"~~", "s"}, {"`", "code"}, {"*", "em"}, {"_", "em"}} {
			if !strings.HasPrefix(text, mark.token) {
				continue
			}
			rest := text[len(mark.token):]
			end := strings.Index(rest, mark.token)
			if end > 0 {
				inner := html.EscapeString(rest[:end])
				if mark.tag != "code" {
					inner = inlineMarkdown(rest[:end], depth+1)
				}
				out.WriteString("<" + mark.tag + ">" + inner + "</" + mark.tag + ">")
				text = rest[end+len(mark.token):]
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		end := strings.IndexAny(text[1:], "\\[!*_~`")
		if end < 0 {
			out.WriteString(html.EscapeString(text))
			break
		}
		end++
		out.WriteString(html.EscapeString(text[:end]))
		text = text[end:]
	}
	return out.String()
}
