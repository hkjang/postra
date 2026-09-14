package webui

import (
	"net/http"

	"postra/internal/platform/mailhtml"
)

type mailEditorData struct {
	Format string
	Text   string
	HTML   string
}

func editorFromForm(r *http.Request) mailEditorData {
	format := "plain"
	if r.FormValue("body_format") == "html" {
		format = "html"
	}
	return mailEditorData{Format: format, Text: r.FormValue("body"), HTML: r.FormValue("body_html")}
}

// Return a string so html/template escapes the srcdoc attribute. The sandbox
// and document CSP isolate even legacy drafts from the application page.
func outgoingMailDocument(body string) string {
	return `<!doctype html><html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"><style>body{margin:0;padding:24px;background:#fff;color:#172033;font:15px/1.65 Arial,sans-serif;overflow-wrap:anywhere}table{max-width:100%}a{color:#3157d5}blockquote{border-left:3px solid #dfe5ee;margin-left:0;padding-left:16px}pre{white-space:pre-wrap}</style></head><body>` + mailhtml.Sanitize(body) + `</body></html>`
}
