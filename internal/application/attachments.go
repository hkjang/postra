package application

import (
	"context"
	"io"
	"regexp"
	"strings"

	"postra/internal/domain"
)

// maxAttachmentTextBytes bounds how much attachment content is read for text
// extraction.
const maxAttachmentTextBytes = 5 << 20

// AttachmentText is the extracted plain text of an attachment.
type AttachmentText struct {
	AttachmentID string `json:"attachment_id"`
	Name         string `json:"name"`
	MIMEType     string `json:"mime_type"`
	Supported    bool   `json:"supported"`
	Truncated    bool   `json:"truncated"`
	Text         string `json:"text,omitempty"`
	Note         string `json:"note,omitempty"`
}

var htmlTagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// ExtractAttachmentText returns the plain text of a text-based attachment
// (text/*, JSON, CSV, HTML-stripped). Binary formats that need OCR or a
// document parser (PDF/Office/images) are reported as unsupported rather than
// returning garbage (§첨부 문서 지능 — 텍스트 추출 범위).
func (a *App) ExtractAttachmentText(ctx context.Context, messageID, attachmentID string) (*AttachmentText, error) {
	att, rc, err := a.GetAttachment(ctx, messageID, attachmentID, false)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	out := &AttachmentText{AttachmentID: att.ID, Name: att.Name, MIMEType: att.MIMEType}
	if !textExtractable(att.MIMEType, att.Name) {
		out.Note = "text extraction is not supported for this type (OCR / document parsing out of scope)"
		return out, nil
	}
	raw, err := io.ReadAll(io.LimitReader(rc, maxAttachmentTextBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAttachmentTextBytes {
		raw = raw[:maxAttachmentTextBytes]
		out.Truncated = true
	}
	text := string(raw)
	if strings.Contains(strings.ToLower(att.MIMEType), "html") || strings.HasSuffix(strings.ToLower(att.Name), ".html") {
		text = stripHTML(text)
	}
	out.Supported = true
	out.Text = strings.TrimSpace(text)
	a.audit(ctx, "attachment_text_extract", "attachment:"+attachmentID, "ok", att.MIMEType)
	return out, nil
}

// SummarizeAttachment extracts an attachment's text and runs the document
// summary analysis over it (respecting the AI policy).
func (a *App) SummarizeAttachment(ctx context.Context, messageID, attachmentID string) (*domain.Analysis, error) {
	at, err := a.ExtractAttachmentText(ctx, messageID, attachmentID)
	if err != nil {
		return nil, err
	}
	if !at.Supported || strings.TrimSpace(at.Text) == "" {
		return nil, userErrf("attachment %q has no extractable text to summarize", attachmentID)
	}
	return a.runAnalysis(ctx, "document_summary", "attachment", attachmentID, "", at.Text)
}

func textExtractable(mimeType, name string) bool {
	m := strings.ToLower(mimeType)
	if strings.HasPrefix(m, "text/") ||
		strings.Contains(m, "json") || strings.Contains(m, "xml") ||
		strings.Contains(m, "csv") || strings.Contains(m, "html") ||
		strings.Contains(m, "markdown") {
		return true
	}
	n := strings.ToLower(name)
	for _, ext := range []string{".txt", ".md", ".csv", ".json", ".xml", ".html", ".htm", ".log", ".yaml", ".yml"} {
		if strings.HasSuffix(n, ext) {
			return true
		}
	}
	return false
}

func stripHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.Join(strings.Fields(s), " ")
}

// AttachmentsTextForIndex concatenates the extractable text of a message's
// attachments, bounded by budget characters. Mail often carries its substance
// in an attached document, so leaving that text out of the semantic index and
// the RAG context makes those messages effectively unsearchable and
// unanswerable. Best-effort: unsupported or unreadable attachments are skipped.
func (a *App) AttachmentsTextForIndex(ctx context.Context, messageID string, budget int) string {
	if budget <= 0 {
		return ""
	}
	atts, err := a.Store.ListAttachments(ctx, userIDFrom(ctx), messageID)
	if err != nil || len(atts) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, att := range atts {
		if sb.Len() >= budget {
			break
		}
		if att.StorageURI == "" || att.ScanStatus == domain.ScanBlocked {
			continue // never re-read content we deliberately did not retain
		}
		if !textExtractable(att.MIMEType, att.Name) {
			continue
		}
		at, err := a.ExtractAttachmentText(ctx, messageID, att.ID)
		if err != nil || at == nil || !at.Supported {
			continue
		}
		text := strings.TrimSpace(at.Text)
		if text == "" {
			continue
		}
		remaining := budget - sb.Len()
		sb.WriteString("\n[attachment: " + att.Name + "]\n")
		sb.WriteString(truncateRunes(text, remaining))
	}
	return sb.String()
}
