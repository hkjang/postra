package application

import (
	"context"
	"fmt"
	"io"

	"postra/internal/domain"
)

func (a *App) Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	q.UserID = DefaultUserID
	return a.Store.Search(ctx, q)
}

type MessageView struct {
	Message     domain.Message      `json:"message"`
	Body        *domain.MessageBody `json:"body,omitempty"`
	Attachments []domain.Attachment `json:"attachments,omitempty"`
	// Score and Reason are populated by semantic search (§7 결과 설명).
	Score  float64 `json:"score,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

func (a *App) GetMessage(ctx context.Context, id string, includeBody bool) (*MessageView, error) {
	m, err := a.Store.GetMessage(ctx, DefaultUserID, id)
	if err != nil {
		return nil, err
	}
	v := &MessageView{Message: *m}
	if includeBody {
		if b, err := a.Store.GetBody(ctx, DefaultUserID, id); err == nil {
			v.Body = b
		}
		if atts, err := a.Store.ListAttachments(ctx, DefaultUserID, id); err == nil {
			v.Attachments = atts
		}
	}
	return v, nil
}

// GetRawMessage streams the original RFC822 bytes; every access is audited
// (§18.3 "메일 원문 조회").
func (a *App) GetRawMessage(ctx context.Context, id string) (io.ReadCloser, error) {
	m, err := a.Store.GetMessage(ctx, DefaultUserID, id)
	if err != nil {
		return nil, err
	}
	a.audit(ctx, "message_raw_read", "message:"+id, "ok", "")
	return a.Objects.Get(m.RawURI)
}

type ThreadView struct {
	ThreadID string        `json:"thread_id"`
	Messages []MessageView `json:"messages"`
}

func (a *App) GetThread(ctx context.Context, threadID string, includeBodies bool) (*ThreadView, error) {
	msgs, err := a.Store.GetThreadMessages(ctx, DefaultUserID, threadID)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, userErrf("thread %s not found or empty", threadID)
	}
	tv := &ThreadView{ThreadID: threadID}
	for _, m := range msgs {
		mv := MessageView{Message: m}
		if includeBodies {
			if b, err := a.Store.GetBody(ctx, DefaultUserID, m.ID); err == nil {
				mv.Body = b
			}
		}
		tv.Messages = append(tv.Messages, mv)
	}
	return tv, nil
}

func (a *App) ListAttachments(ctx context.Context, messageID string) ([]domain.Attachment, error) {
	if _, err := a.Store.GetMessage(ctx, DefaultUserID, messageID); err != nil {
		return nil, err
	}
	return a.Store.ListAttachments(ctx, DefaultUserID, messageID)
}

// GetAttachment streams an attachment. ack must be true to download a
// quarantined/suspect attachment; blocked attachments are never available.
func (a *App) GetAttachment(ctx context.Context, messageID, attachmentID string, ack bool) (*domain.Attachment, io.ReadCloser, error) {
	atts, err := a.ListAttachments(ctx, messageID)
	if err != nil {
		return nil, nil, err
	}
	for _, at := range atts {
		if at.ID == attachmentID {
			// Blocked attachments were never retained (MIME-012).
			if at.StorageURI == "" || at.ScanStatus == domain.ScanBlocked {
				a.audit(ctx, "attachment_download", "attachment:"+attachmentID, "denied", at.ScanDetail)
				return nil, nil, userErrf("attachment %q was blocked by policy and is not available: %s", at.Name, at.ScanDetail)
			}
			// Quarantined/suspect content requires explicit acknowledgement.
			if (at.ScanStatus == domain.ScanQuarantined || at.ScanStatus == domain.ScanSuspect) && !ack {
				a.audit(ctx, "attachment_download", "attachment:"+attachmentID, "denied", "ack required")
				return nil, nil, userErrf("attachment %q is %s (%s); re-request with acknowledgement to download",
					at.Name, at.ScanStatus, at.ScanDetail)
			}
			rc, err := a.Objects.Get(at.StorageURI)
			if err != nil {
				return nil, nil, err
			}
			a.audit(ctx, "attachment_download", "attachment:"+attachmentID, "ok",
				fmt.Sprintf("%s (%s)", at.Name, at.ScanStatus))
			return &at, rc, nil
		}
	}
	return nil, nil, userErrf("attachment %s not found on message %s", attachmentID, messageID)
}

func (a *App) SearchAudit(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	return a.Store.SearchAudit(ctx, DefaultUserID, limit)
}

// PolicySnapshot returns the currently applied, non-sensitive policy for the
// MCP resource policy://mail/current. It never includes secrets or keys.
func (a *App) PolicySnapshot() map[string]any {
	return map[string]any{
		"allow_insecure_mail":    a.Cfg.AllowInsecureMail,
		"allow_private_hosts":    a.Cfg.AllowPrivateHosts,
		"encrypt_at_rest":        a.Cfg.EncryptAtRest,
		"ai_allow_external":      a.Cfg.AI.AllowExternal,
		"ai_model":               a.Cfg.AI.Model,
		"send_requires_approval": true,
		"server_delete_default":  "retain",
		"max_message_bytes":      a.Cfg.Sync.MaxMessageBytes,
		"max_per_sync":           a.Cfg.Sync.MaxPerSync,
	}
}
