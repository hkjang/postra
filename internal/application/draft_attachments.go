package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"strings"

	"postra/internal/adapters/mailparse"
	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/mailrender"
	"postra/internal/platform/mailhtml"
)

const MaxOutboundAttachmentBytes = 64 << 20

type AddDraftAttachmentInput struct {
	DraftID    string `json:"draft_id"`
	Name       string `json:"name"`
	DataBase64 string `json:"data_base64"`
	Inline     bool   `json:"inline,omitempty"`
}

func (a *App) AddDraftAttachment(ctx context.Context, in AddDraftAttachmentInput) (*DraftView, error) {
	if err := a.checkComposeMCPScopes(ctx, "mail.draft"); err != nil {
		return nil, err
	}
	draft, current, err := a.Store.GetDraft(ctx, userIDFrom(ctx), in.DraftID)
	if err != nil {
		return nil, err
	}
	if draft.Status != domain.DraftOpen {
		return nil, userErrf("attachments can only be changed on an open draft")
	}
	if len(in.DataBase64) > base64.StdEncoding.EncodedLen(MaxOutboundAttachmentBytes) {
		return nil, userErrf("attachment is too large")
	}
	data, err := base64.StdEncoding.DecodeString(in.DataBase64)
	if err != nil {
		return nil, userErrf("attachment data_base64 is invalid")
	}
	if len(data) == 0 || len(data) > MaxOutboundAttachmentBytes {
		return nil, userErrf("attachment must contain 1 to %d bytes", MaxOutboundAttachmentBytes)
	}
	if max := a.SettingInt("attachments.max_count"); max <= 0 || len(current.Attachments) >= max {
		return nil, userErrf("attachment count exceeds administrator limit")
	}
	var total int64
	for _, att := range current.Attachments {
		total += att.Size
	}
	if max := a.SettingInt("attachments.max_bytes"); max <= 0 || total+int64(len(data)) > int64(max) {
		return nil, userErrf("total attachment size exceeds administrator limit")
	}
	name := mailparse.SanitizeFilename(in.Name)
	if name == "" || len([]rune(name)) > 200 {
		return nil, userErrf("attachment filename must contain 1 to 200 characters")
	}
	mimeType, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	verdict := a.ScanAttachment(ctx, domain.ScanInput{Name: name, MIMEType: mimeType, Data: data})
	if verdict.Status != domain.ScanClean || !verdict.StoreContent {
		return nil, userErrf("attachment was rejected by the scanning policy (%s)", verdict.Status)
	}
	if in.Inline {
		if !a.SettingBool("mail.html_enabled") {
			return nil, userErrf("inline images require HTML mail permission")
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || (format != "png" && format != "jpeg" && format != "gif") || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
			return nil, userErrf("inline images must be valid PNG, JPEG or GIF images of at most 40 megapixels")
		}
	}
	uri, hash, size, err := a.Objects.Put("draftatt", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	attachment := domain.DraftAttachment{ID: persistence.NewID("att"), Name: name, MIMEType: mimeType, Size: size, Hash: hash, StorageURI: uri, Inline: in.Inline, ScanStatus: verdict.Status}
	version := *current
	version.Author = "user"
	version.Attachments = append(append([]domain.DraftAttachment{}, current.Attachments...), attachment)
	if in.Inline {
		attachment.ContentID = "postra-" + attachment.ID + "@postra.local"
		version.Attachments[len(version.Attachments)-1] = attachment
		if version.BodyHTML == "" {
			fragment, _ := mailrender.Fragment(mailrender.Input{Body: version.BodyText, Format: "text"})
			version.BodyHTML = fragment.BodyHTML
		}
		version.BodyHTML = a.sanitizeMailHTML(version.BodyHTML + `<p><img src="cid:` + attachment.ContentID + `" alt="` + html.EscapeString(name) + `" style="max-width:100%;height:auto"></p>`)
		version.BodyText = mailhtml.PlainText(version.BodyHTML)
	}
	next, err := a.Store.AddDraftVersion(ctx, userIDFrom(ctx), draft.ID, &version)
	if err != nil {
		return nil, err
	}
	draft.CurrentVersion = next
	a.audit(ctx, "draft_attachment_add", "draft:"+draft.ID, "ok", "attachment="+attachment.ID)
	return &DraftView{Draft: *draft, Version: version}, nil
}

func (a *App) RemoveDraftAttachment(ctx context.Context, draftID, attachmentID string) (*DraftView, error) {
	if err := a.checkComposeMCPScopes(ctx, "mail.draft"); err != nil {
		return nil, err
	}
	draft, current, err := a.Store.GetDraft(ctx, userIDFrom(ctx), draftID)
	if err != nil {
		return nil, err
	}
	if draft.Status != domain.DraftOpen {
		return nil, userErrf("attachments can only be changed on an open draft")
	}
	version := *current
	version.Author = "user"
	version.Attachments = []domain.DraftAttachment{}
	found := false
	for _, attachment := range current.Attachments {
		if attachment.ID != attachmentID {
			version.Attachments = append(version.Attachments, attachment)
			continue
		}
		found = true
		if attachment.Inline {
			version.BodyHTML = mailrender.RemoveInlineImage(version.BodyHTML, attachment.ContentID)
			version.BodyText = mailhtml.PlainText(version.BodyHTML)
		}
	}
	if !found {
		return nil, domain.ErrNotFound
	}
	next, err := a.Store.AddDraftVersion(ctx, userIDFrom(ctx), draft.ID, &version)
	if err != nil {
		return nil, err
	}
	draft.CurrentVersion = next
	a.audit(ctx, "draft_attachment_remove", "draft:"+draft.ID, "ok", "attachment="+attachmentID)
	// Retain immutable blobs for prior versions and sent messages. This is a
	// logical removal from this draft, not destruction of mail history.
	return &DraftView{Draft: *draft, Version: version}, nil
}

func (a *App) GetDraftAttachment(ctx context.Context, draftID, attachmentID string, version int) (*domain.DraftAttachment, []byte, error) {
	if err := a.checkComposeMCPScopes(ctx, "mail.draft"); err != nil {
		return nil, nil, err
	}
	_, current, err := a.Store.GetDraft(ctx, userIDFrom(ctx), draftID)
	if err != nil {
		return nil, nil, err
	}
	if version > 0 {
		current, err = a.Store.GetDraftVersion(ctx, userIDFrom(ctx), draftID, version)
		if err != nil {
			return nil, nil, err
		}
	}
	for _, attachment := range current.Attachments {
		if attachment.ID == attachmentID {
			data, err := a.readDraftAttachment(ctx, attachment)
			return &attachment, data, err
		}
	}
	return nil, nil, domain.ErrNotFound
}

func (a *App) readDraftAttachment(ctx context.Context, attachment domain.DraftAttachment) ([]byte, error) {
	if attachment.Size <= 0 || attachment.Size > MaxOutboundAttachmentBytes || attachment.StorageURI == "" || attachment.ScanStatus != domain.ScanClean {
		return nil, userErrf("attachment is not approved for use")
	}
	reader, err := a.Objects.Get(attachment.StorageURI)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, attachment.Size+1))
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	if int64(len(data)) != attachment.Size || hex.EncodeToString(hash[:]) != attachment.Hash {
		return nil, userErrf("attachment integrity verification failed")
	}
	verdict := a.ScanAttachment(ctx, domain.ScanInput{Name: attachment.Name, MIMEType: attachment.MIMEType, Data: data})
	if verdict.Status != domain.ScanClean || !verdict.StoreContent {
		return nil, userErrf("attachment is blocked by current scanning policy (%s)", verdict.Status)
	}
	return data, nil
}

func (a *App) validateDraftAttachments(v *domain.DraftVersion) error {
	var total int64
	seen := map[string]bool{}
	cids := map[string]bool{}
	for _, att := range v.Attachments {
		if seen[att.ID] || att.Size <= 0 || att.Size > MaxOutboundAttachmentBytes || att.ScanStatus != domain.ScanClean || strings.ContainsAny(att.Name+att.MIMEType+att.ContentID, "\r\n\x00") {
			return userErrf("invalid or unapproved draft attachment")
		}
		seen[att.ID] = true
		if att.Inline {
			if att.ContentID != "postra-"+att.ID+"@postra.local" || cids[att.ContentID] {
				return userErrf("invalid inline attachment reference")
			}
			cids[att.ContentID] = true
		}
		total += att.Size
		if att.Inline && !a.SettingBool("mail.html_enabled") {
			return userErrf("inline attachments require HTML mail permission")
		}
	}
	if len(v.Attachments) > 0 && (len(v.Attachments) > a.SettingInt("attachments.max_count") || total > int64(a.SettingInt("attachments.max_bytes"))) {
		return userErrf("attachments exceed current administrator limits")
	}
	_, references := mailrender.InlineImages(v.BodyHTML)
	for _, id := range references {
		if !cids[id] {
			return userErrf("mail body references an unavailable inline attachment")
		}
	}
	return nil
}

func attachmentPolicyText(attachment domain.DraftAttachment, data []byte) string {
	text := "\n" + attachment.Name
	if strings.HasPrefix(attachment.MIMEType, "text/") || strings.HasPrefix(attachment.MIMEType, "application/json") || strings.HasPrefix(attachment.MIMEType, "application/xml") {
		text += "\n" + string(data)
	}
	return text
}

func (a *App) draftAttachmentsPolicyText(ctx context.Context, v *domain.DraftVersion) (string, error) {
	var text strings.Builder
	for _, attachment := range v.Attachments {
		data, err := a.readDraftAttachment(ctx, attachment)
		if err != nil {
			return "", err
		}
		text.WriteString(attachmentPolicyText(attachment, data))
	}
	return text.String(), nil
}

func (a *App) enforceDraftDLP(ctx context.Context, acc *domain.MailAccount, v *domain.DraftVersion) error {
	text, err := a.draftAttachmentsPolicyText(ctx, v)
	if err != nil {
		return err
	}
	copy := *v
	copy.BodyText, copy.BodyHTML = draftPolicyText(v)+text, ""
	return a.enforceDLP(acc, &copy)
}
