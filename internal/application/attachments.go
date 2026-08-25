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

// indexReadCapBytes hard-caps a single attachment read on the indexing path,
// independent of the caller's character budget.
const indexReadCapBytes = 256 << 10

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
// unanswerable.
//
// This deliberately does NOT reuse ExtractAttachmentText/GetAttachment, which
// exist to serve a person who asked for a file. Background indexing differs on
// three points that matter:
//   - it reads only the prefix it will actually keep, instead of pulling up to
//     maxAttachmentTextBytes per attachment into memory (an embedding batch of
//     attachment-heavy mail would otherwise allocate hundreds of megabytes);
//   - it never records an attachment_download audit event, which would falsely
//     attribute a download to a user who did nothing;
//   - it indexes only clean attachments, so quarantined or suspect content is
//     not read behind the acknowledgement gate that guards manual downloads.
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
		remaining := budget - len([]rune(sb.String()))
		if remaining <= 0 {
			break
		}
		// Only retained, clean, text-bearing attachments are indexed.
		if att.StorageURI == "" || att.ScanStatus != domain.ScanClean {
			continue
		}
		if !textExtractable(att.MIMEType, att.Name) {
			continue
		}
		text := a.attachmentTextHead(att.StorageURI, att.MIMEType, att.Name, remaining)
		if text == "" {
			continue
		}
		sb.WriteString("\n[attachment: " + att.Name + "]\n")
		sb.WriteString(text)
	}
	return sb.String()
}

// attachmentTextHead reads just enough of an attachment to yield `budget`
// characters of text, so indexing a 50 MB log file costs kilobytes, not
// megabytes. UTF-8 needs at most 4 bytes per rune, and HTML only shrinks once
// tags are stripped, so a modest multiplier plus slack always covers the budget.
func (a *App) attachmentTextHead(storageURI, mimeType, name string, budget int) string {
	readBytes := budget*4 + 4096
	if readBytes > indexReadCapBytes {
		readBytes = indexReadCapBytes
	}
	rc, err := a.Objects.Get(storageURI)
	if err != nil {
		return ""
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, int64(readBytes)))
	if err != nil || len(raw) == 0 {
		return ""
	}
	text := string(raw)
	if strings.Contains(strings.ToLower(mimeType), "html") || strings.HasSuffix(strings.ToLower(name), ".html") {
		text = stripHTML(text)
	}
	// A truncated read can end mid-rune; drop any invalid tail.
	text = strings.ToValidUTF8(text, "")
	return truncateRunes(strings.TrimSpace(text), budget)
}
