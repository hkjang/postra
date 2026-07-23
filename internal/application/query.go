package application

import (
	"context"
	"fmt"
	"io"
	"sort"

	"postra/internal/domain"
	"postra/internal/platform/mask"
)

func (a *App) Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	q.UserID = userIDFrom(ctx)
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
	return a.getMessage(ctx, id, includeBody, false)
}

// GetMessageMasked returns a message with sensitive patterns redacted in the
// body and subject (§7 보안 검색).
func (a *App) GetMessageMasked(ctx context.Context, id string, includeBody bool) (*MessageView, error) {
	return a.getMessage(ctx, id, includeBody, true)
}

func (a *App) getMessage(ctx context.Context, id string, includeBody, doMask bool) (*MessageView, error) {
	userID := userIDFrom(ctx)
	m, err := a.Store.GetMessage(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if doMask {
		m.Subject, _ = mask.Mask(m.Subject)
	}
	v := &MessageView{Message: *m}
	if includeBody {
		if b, err := a.Store.GetBody(ctx, userID, id); err == nil {
			if doMask {
				b.TextBody, _ = mask.Mask(b.TextBody)
				b.HTMLSanitized, _ = mask.Mask(b.HTMLSanitized)
			}
			v.Body = b
		}
		if atts, err := a.Store.ListAttachments(ctx, userID, id); err == nil {
			v.Attachments = atts
		}
	}
	return v, nil
}

// GetRawMessage streams the original RFC822 bytes; every access is audited
// (§18.3 "메일 원문 조회").
func (a *App) GetRawMessage(ctx context.Context, id string) (io.ReadCloser, error) {
	m, err := a.Store.GetMessage(ctx, userIDFrom(ctx), id)
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
	userID := userIDFrom(ctx)
	msgs, err := a.Store.GetThreadMessages(ctx, userID, threadID)
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
			if b, err := a.Store.GetBody(ctx, userID, m.ID); err == nil {
				mv.Body = b
			}
		}
		tv.Messages = append(tv.Messages, mv)
	}
	return tv, nil
}

func (a *App) ListAttachments(ctx context.Context, messageID string) ([]domain.Attachment, error) {
	if _, err := a.Store.GetMessage(ctx, userIDFrom(ctx), messageID); err != nil {
		return nil, err
	}
	return a.Store.ListAttachments(ctx, userIDFrom(ctx), messageID)
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
	return a.Store.SearchAudit(ctx, userIDFrom(ctx), limit)
}

// PolicySnapshot returns the currently applied, non-sensitive policy for the
// MCP resource policy://mail/current. It never includes secrets or keys.
func (a *App) PolicySnapshot() map[string]any {
	aiCfg := a.currentAIConfig()
	return map[string]any{
		"allow_insecure_mail":    a.Cfg.AllowInsecureMail,
		"allow_private_hosts":    a.Cfg.AllowPrivateHosts,
		"encrypt_at_rest":        a.Cfg.EncryptAtRest,
		"ai_allow_external":      aiCfg.AllowExternal,
		"ai_model":               aiCfg.Model,
		"send_requires_approval": true,
		"server_delete_default":  "retain",
		"max_message_bytes":      a.Cfg.Sync.MaxMessageBytes,
		"max_per_sync":           a.Cfg.Sync.MaxPerSync,
	}
}

type BatchAction string

const (
	BatchActionDelete    BatchAction = "delete"
	BatchActionArchive   BatchAction = "archive"
	BatchActionUnarchive BatchAction = "unarchive"
	BatchActionImportant BatchAction = "important"
	BatchActionSnooze    BatchAction = "snooze"
)

type BatchUpdateOptions struct {
	MessageIDs   []string    `json:"message_ids"`
	Action       BatchAction `json:"action"`
	SnoozedUntil int64       `json:"snoozed_until,omitempty"`
}

// BatchUpdateMessages performs bulk operations (delete, archive, mark important, snooze) on messages (§P2 메일 UX 확장).
func (a *App) BatchUpdateMessages(ctx context.Context, opts BatchUpdateOptions) (int, error) {
	userID := userIDFrom(ctx)
	if len(opts.MessageIDs) == 0 {
		return 0, userErrf("no message IDs provided")
	}

	count := 0
	for _, id := range opts.MessageIDs {
		switch opts.Action {
		case BatchActionDelete:
			if err := a.LocalDelete(ctx, id); err == nil {
				count++
			}
		case BatchActionArchive, BatchActionUnarchive:
			if m, err := a.Store.GetMessage(ctx, userID, id); err == nil {
				m.IsArchived = (opts.Action == BatchActionArchive)
				if err := a.Store.UpdateMessage(ctx, m); err == nil {
					count++
				}
			}
		case BatchActionImportant:
			if m, err := a.Store.GetMessage(ctx, userID, id); err == nil {
				m.IsImportant = !m.IsImportant
				if err := a.Store.UpdateMessage(ctx, m); err == nil {
					count++
				}
			}
		case BatchActionSnooze:
			if m, err := a.Store.GetMessage(ctx, userID, id); err == nil {
				m.SnoozedUntil = opts.SnoozedUntil
				if err := a.Store.UpdateMessage(ctx, m); err == nil {
					count++
				}
			}
		default:
			return 0, userErrf("unsupported batch action %q", opts.Action)
		}
	}

	a.audit(ctx, "batch_update_messages", "action:"+string(opts.Action), "ok", fmt.Sprintf("count=%d", count))
	return count, nil
}

// GetThreadTimeline returns all messages in a conversation thread formatted for a interactive timeline view (§P2 대화형 타임라인).
func (a *App) GetThreadTimeline(ctx context.Context, threadID string) ([]MessageView, error) {
	if threadID == "" {
		return nil, userErrf("threadID is required")
	}
	userID := userIDFrom(ctx)

	res, err := a.Store.Search(ctx, domain.SearchQuery{
		UserID: userID,
		Limit:  100,
	})
	if err != nil {
		return nil, err
	}

	var threadMsgs []domain.Message
	for _, m := range res.Messages {
		if m.ThreadID == threadID || m.ID == threadID {
			threadMsgs = append(threadMsgs, m)
		}
	}

	sort.Slice(threadMsgs, func(i, j int) bool {
		return threadMsgs[i].Date < threadMsgs[j].Date
	})

	var timeline []MessageView
	for _, m := range threadMsgs {
		v, err := a.GetMessage(ctx, m.ID, true)
		if err == nil && v != nil {
			timeline = append(timeline, *v)
		}
	}

	return timeline, nil
}
