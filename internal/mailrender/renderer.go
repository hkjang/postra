// Package mailrender turns authored text, Markdown and HTML into deterministic,
// safe email alternatives. It has no provider, storage, clock or network access.
package mailrender

import (
	"fmt"
	"html"
	"regexp"
	"strings"

	"postra/internal/platform/mailhtml"
)

const MaxInputBytes = 1 << 20

type Input struct {
	Body           string `json:"body,omitempty"`
	BodyHTML       string `json:"body_html,omitempty"`
	Format         string `json:"format,omitempty"`
	Template       string `json:"template,omitempty"`
	SignatureHTML  string `json:"-"`
	SignatureText  string `json:"-"`
	SignatureID    string `json:"-"`
	StripSignature bool   `json:"-"`
	ImagePolicy    string `json:"-"`
	ImageProxyURL  string `json:"-"`
}

type Output struct {
	BodyHTML string   `json:"body_html,omitempty"`
	BodyText string   `json:"body_text"`
	Format   string   `json:"format"`
	Template string   `json:"template"`
	Warnings []string `json:"warnings,omitempty"`
}

var htmlTag = regexp.MustCompile(`(?i)</?(?:p|div|span|br|h[1-6]|table|ul|ol|li|strong|b|em|i|a|blockquote|pre|hr|img)(?:\s[^>]*|\s*/?)>`)
var markdownSignal = regexp.MustCompile("(?m)^ {0,3}(?:#{1,6} |[-*+] |[0-9]+[.)] |>|```|~~~)|\\*\\*[^*]+\\*\\*|\\[[^\\]]+\\]\\([^)]+\\)")
var signatureMarker = regexp.MustCompile(`^sig_[A-Za-z0-9_-]{1,120}$`)

func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return strings.ToValidUTF8(body, "�")
}

// Fragment applies input-format detection and sanitization without adding a
// theme or signature. Signatures reuse this path instead of trusting raw HTML.
func Fragment(in Input) (Output, error) {
	if len(in.Body)+len(in.BodyHTML) > MaxInputBytes {
		return Output{}, fmt.Errorf("mail body exceeds %d bytes", MaxInputBytes)
	}
	format := strings.ToLower(strings.TrimSpace(in.Format))
	if format == "" {
		format = "auto"
	}
	body := normalizeBody(in.Body)
	if format == "auto" {
		switch {
		case strings.TrimSpace(in.BodyHTML) != "":
			format, body = "html", normalizeBody(in.BodyHTML)
		case htmlTag.MatchString(body):
			format = "html"
		case markdownSignal.MatchString(body):
			format = "markdown"
		default:
			format = "text"
		}
	} else if format == "html" && in.BodyHTML != "" {
		body = normalizeBody(in.BodyHTML)
	}
	var rendered string
	switch format {
	case "text":
		rendered = textHTML(body)
	case "markdown":
		rendered = markdownHTML(body)
	case "html":
		rendered = body
	default:
		return Output{}, fmt.Errorf("unsupported mail format %q (text, markdown, html, auto)", format)
	}
	rendered = mailhtml.SanitizeWithImages(rendered, in.ImagePolicy, in.ImageProxyURL)
	return Output{BodyHTML: rendered, BodyText: mailhtml.PlainText(rendered), Format: format}, nil
}

func Render(in Input) (Output, error) {
	if in.ImagePolicy != "" && in.ImagePolicy != "block" && in.ImagePolicy != "allow" && in.ImagePolicy != "proxy" {
		return Output{}, fmt.Errorf("invalid outbound image policy")
	}
	if in.ImagePolicy == "proxy" && !mailhtml.ValidImageProxy(in.ImageProxyURL) {
		return Output{}, fmt.Errorf("an HTTPS image proxy URL is required")
	}
	if len(in.Body)+len(in.BodyHTML)+len(in.SignatureHTML)+len(in.SignatureText) > MaxInputBytes {
		return Output{}, fmt.Errorf("mail body and signature exceed %d bytes", MaxInputBytes)
	}
	if in.StripSignature || in.SignatureHTML != "" || in.SignatureText != "" {
		if in.BodyHTML != "" {
			before := in.BodyHTML
			in.BodyHTML = StripSignatureHTML(in.BodyHTML)
			if before != in.BodyHTML && in.Format == "text" {
				in.Body = mailhtml.PlainText(in.BodyHTML)
			}
		} else if in.Format == "html" || (in.Format == "auto" && htmlTag.MatchString(in.Body)) {
			in.Body = StripSignatureHTML(in.Body)
		}
	}
	out, err := Fragment(in)
	if err != nil {
		return Output{}, err
	}
	template := strings.ToLower(strings.TrimSpace(in.Template))
	if template == "" {
		template = "clean"
	}
	if !ValidTemplate(template) {
		return Output{}, fmt.Errorf("unsupported mail template %q", template)
	}
	out.Template = template
	signature := mailhtml.SanitizeWithImages(in.SignatureHTML, in.ImagePolicy, in.ImageProxyURL)
	if signature == "" && in.SignatureText != "" {
		signature = textHTML(normalizeBody(in.SignatureText))
	}
	if template == "plain" {
		if out.Format == "text" {
			out.BodyText = strings.TrimSpace(normalizeBody(in.Body))
		}
		if signature != "" {
			// Plain mail has no HTML provenance marker. Remove only an exact
			// generated suffix for this same signature, never arbitrary body text.
			suffix := "\n\n-- \n" + mailhtml.PlainText(signature)
			out.BodyText = strings.TrimSuffix(out.BodyText, suffix)
			out.BodyText = strings.TrimSpace(out.BodyText + "\n\n-- \n" + mailhtml.PlainText(signature))
		}
		out.BodyHTML = ""
		return out, nil
	}
	if signature != "" {
		marker := ""
		if signatureMarker.MatchString(in.SignatureID) {
			marker = ` data-postra-signature="` + in.SignatureID + `"`
		}
		out.BodyHTML += `<div` + marker + ` style="margin-top:24px;padding-top:16px;border-top:1px solid #dfe5ee;font-size:13px;color:#596579">` + signature + `</div>`
	}
	out.BodyHTML = mailhtml.SanitizeWithImages(themeHTML(out.BodyHTML, template), in.ImagePolicy, in.ImageProxyURL)
	// Always derive the canonical alternative from the final transmitted HTML,
	// including link destinations and signatures (also used by DLP and hashing).
	out.BodyText = mailhtml.PlainText(out.BodyHTML)
	return out, nil
}

func textHTML(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	var result strings.Builder
	for _, paragraph := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(paragraph) == "" {
			continue
		}
		result.WriteString("<p>")
		result.WriteString(strings.ReplaceAll(html.EscapeString(paragraph), "\n", "<br>"))
		result.WriteString("</p>")
	}
	return result.String()
}
