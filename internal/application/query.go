package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"postra/internal/domain"
	"postra/internal/platform/mask"
	"postra/internal/platform/metrics"
)

// loadBody fetches a message body, turning a decode/decrypt failure into a
// visible "unavailable" marker (plus a metric) rather than a silent blank. A
// key change on restart made GetBody error, and callers used to swallow it —
// so the message showed a subject with an empty body ("제목만 수집된" 것처럼
// 보이던 증상). ErrNotFound means there is genuinely no body row (headers-only)
// and stays nil.
func (a *App) loadBody(ctx context.Context, userID, messageID string) *domain.MessageBody {
	b, err := a.Store.GetBody(ctx, userID, messageID)
	if err == nil {
		return b
	}
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	metrics.BodyUnavailable.Inc()
	slog.Warn("message body unavailable", "message", messageID, "err", err)
	// Stable message (no per-message ID) so all such failures collapse into one
	// incident with a rising count instead of flooding the admin view.
	a.recordIncident(domain.SeverityWarning, "message-body",
		"본문 복호화/디코딩 실패 (at-rest 키 불일치 가능)", "message="+messageID+" err="+err.Error())
	return &domain.MessageBody{
		MessageID:         messageID,
		Unavailable:       true,
		UnavailableReason: "본문을 불러올 수 없습니다 (at-rest 키 불일치 가능) — 본문 재동기화가 필요합니다.",
	}
}

func (a *App) Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	q.UserID = userIDFrom(ctx)
	result, err := a.Store.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	result.Messages = nilToEmpty(result.Messages)
	ids := make([]string, 0, len(result.Messages))
	for _, message := range result.Messages {
		ids = append(ids, message.ID)
	}
	if bodies, err := a.Store.BodyTextBatch(ctx, q.UserID, ids); err == nil {
		result.Snippets = make(map[string]string, len(bodies))
		for id, body := range bodies {
			text := []rune(strings.Join(strings.Fields(body), " "))
			if len(text) > 200 {
				text = append(text[:200], '…')
			}
			result.Snippets[id] = string(text)
		}
	}
	return result, nil
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
	return a.getMessage(ctx, id, includeBody, false, false)
}

// GetMessageMasked returns a message with sensitive patterns redacted in the
// body and subject (§7 보안 검색).
func (a *App) GetMessageMasked(ctx context.Context, id string, includeBody bool) (*MessageView, error) {
	return a.getMessage(ctx, id, includeBody, true, false)
}

func (a *App) getMessage(ctx context.Context, id string, includeBody, doMask, allowImagesOnce bool) (*MessageView, error) {
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
		if b := a.loadBody(ctx, userID, id); b != nil {
			a.prepareReceivedBody(ctx, *m, b, allowImagesOnce)
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
			mv.Body = a.loadBody(ctx, userID, m.ID)
			a.prepareReceivedBody(ctx, m, mv.Body, false)
		}
		tv.Messages = append(tv.Messages, mv)
	}
	return tv, nil
}

func (a *App) ListAttachments(ctx context.Context, messageID string) ([]domain.Attachment, error) {
	if _, err := a.Store.GetMessage(ctx, userIDFrom(ctx), messageID); err != nil {
		return nil, err
	}
	return listResult(a.Store.ListAttachments(ctx, userIDFrom(ctx), messageID))
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
	events, err := a.Store.SearchAudit(ctx, userIDFrom(ctx), limit)
	for i := range events {
		events[i] = safeAuditDiagnostic(events[i])
	}
	return listResult(events, err)
}

// PolicySnapshot returns the currently applied, non-sensitive policy for the
// MCP resource policy://mail/current. It never includes secrets or keys.
func (a *App) PolicySnapshot() map[string]any {
	aiCfg := a.currentAIConfig()
	return map[string]any{
		"allow_insecure_mail":    a.EffectiveConfig().AllowInsecureMail,
		"allow_private_hosts":    a.EffectiveConfig().AllowPrivateHosts,
		"encrypt_at_rest":        a.Cfg.EncryptAtRest,
		"ai_allow_external":      aiCfg.AllowExternal,
		"ai_model":               aiCfg.Model,
		"send_requires_approval": true,
		"server_delete_default":  "retain",
		"max_message_bytes":      a.EffectiveConfig().Sync.MaxMessageBytes,
		"max_per_sync":           a.EffectiveConfig().Sync.MaxPerSync,
	}
}

type BatchAction string

const (
	BatchActionDelete          BatchAction = "delete"
	BatchActionArchive         BatchAction = "archive"
	BatchActionUnarchive       BatchAction = "unarchive"
	BatchActionMarkImportant   BatchAction = "mark_important"
	BatchActionUnmarkImportant BatchAction = "unmark_important"
	BatchActionMarkRead        BatchAction = "mark_read"
	BatchActionMarkUnread      BatchAction = "mark_unread"
	BatchActionSnooze          BatchAction = "snooze"
	BatchActionUnsnooze        BatchAction = "unsnooze"
	BatchActionAddLabel        BatchAction = "add_label"
	BatchActionRemoveLabel     BatchAction = "remove_label"
	BatchActionLegalHold       BatchAction = "legal_hold"
	BatchActionLegalUnhold     BatchAction = "legal_unhold"
	// BatchActionImportant is a deprecated toggle kept for backward
	// compatibility. Prefer the explicit mark_important/unmark_important so a
	// retry is idempotent instead of flipping state.
	BatchActionImportant BatchAction = "important"
)

type BatchUpdateOptions struct {
	MessageIDs   []string    `json:"message_ids"`
	Action       BatchAction `json:"action"`
	SnoozedUntil int64       `json:"snoozed_until,omitempty"`
	Label        string      `json:"label,omitempty"`
}

// BatchItemResult is the outcome for a single message in a batch operation.
type BatchItemResult struct {
	MessageID string `json:"message_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// BatchResult reports exactly which messages succeeded or failed and why, so a
// caller can retry the failures rather than trusting an aggregate count.
type BatchResult struct {
	Action       BatchAction       `json:"action"`
	Requested    int               `json:"requested"`
	Succeeded    int               `json:"succeeded"`
	Failed       int               `json:"failed"`
	SucceededIDs []string          `json:"succeeded_ids,omitempty"`
	Results      []BatchItemResult `json:"results"`
}

// BatchUpdateMessages performs bulk operations on messages and returns a
// per-message result set (§P2 메일 UX 확장). Actions are explicit and
// idempotent (mark_important vs unmark_important) so retries converge.
func (a *App) BatchUpdateMessages(ctx context.Context, opts BatchUpdateOptions) (*BatchResult, error) {
	if p, ok := PrincipalFrom(ctx); ok && p.IsMCPScoped() && (opts.Action == BatchActionLegalHold || opts.Action == BatchActionLegalUnhold) {
		if !p.IsAdmin() {
			return nil, &domain.PublicError{Code: "forbidden", Message: "법적 보존 정책 변경에는 관리자 권한이 필요합니다.", Status: 403}
		}
		if err := a.CheckMCPPermissionScopes(ctx, "admin.write"); err != nil {
			return nil, err
		}
	}
	if len(opts.MessageIDs) == 0 {
		return nil, userErrf("no message IDs provided")
	}
	switch opts.Action {
	case BatchActionDelete, BatchActionArchive, BatchActionUnarchive,
		BatchActionMarkRead, BatchActionMarkUnread,
		BatchActionMarkImportant, BatchActionUnmarkImportant, BatchActionImportant,
		BatchActionSnooze, BatchActionUnsnooze, BatchActionAddLabel, BatchActionRemoveLabel,
		BatchActionLegalHold, BatchActionLegalUnhold:
	default:
		return nil, userErrf("unsupported batch action %q", opts.Action)
	}
	if opts.Action == BatchActionSnooze && opts.SnoozedUntil <= 0 {
		return nil, userErrf("snooze requires snoozed_until (unix seconds)")
	}
	if (opts.Action == BatchActionAddLabel || opts.Action == BatchActionRemoveLabel) && strings.TrimSpace(opts.Label) == "" {
		return nil, userErrf("%s requires a non-empty label", opts.Action)
	}

	res := &BatchResult{Action: opts.Action, Requested: len(opts.MessageIDs)}
	for _, id := range opts.MessageIDs {
		err := a.applyBatchAction(ctx, id, opts)
		item := BatchItemResult{MessageID: id, OK: err == nil}
		if err != nil {
			_, public := PublicError(ctx, err)
			item.Error = public.Message
			res.Failed++
		} else {
			res.Succeeded++
			res.SucceededIDs = append(res.SucceededIDs, id)
		}
		res.Results = append(res.Results, item)
	}

	result := "ok"
	if res.Failed > 0 {
		result = "partial"
		if res.Succeeded == 0 {
			result = "error"
		}
	}
	a.audit(ctx, "batch_update_messages", "action:"+string(opts.Action), result,
		fmt.Sprintf("requested=%d ok=%d failed=%d", res.Requested, res.Succeeded, res.Failed))
	return res, nil
}

func (a *App) applyBatchAction(ctx context.Context, id string, opts BatchUpdateOptions) error {
	userID := userIDFrom(ctx)
	if opts.Action == BatchActionDelete {
		return a.LocalDelete(ctx, id)
	}
	m, err := a.Store.GetMessage(ctx, userID, id)
	if err != nil {
		return err
	}
	switch opts.Action {
	case BatchActionArchive:
		m.IsArchived = true
	case BatchActionUnarchive:
		m.IsArchived = false
	case BatchActionMarkRead:
		m.IsRead = true
	case BatchActionMarkUnread:
		m.IsRead = false
	case BatchActionMarkImportant:
		m.IsImportant = true
	case BatchActionUnmarkImportant:
		m.IsImportant = false
	case BatchActionImportant:
		m.IsImportant = !m.IsImportant // deprecated toggle
	case BatchActionSnooze:
		m.SnoozedUntil = opts.SnoozedUntil
	case BatchActionUnsnooze:
		m.SnoozedUntil = 0
	case BatchActionAddLabel:
		m.Labels = addLabel(m.Labels, opts.Label)
	case BatchActionRemoveLabel:
		m.Labels = removeLabel(m.Labels, opts.Label)
	case BatchActionLegalHold:
		m.LegalHold = true
	case BatchActionLegalUnhold:
		m.LegalHold = false
	}
	return a.Store.UpdateMessage(ctx, m)
}

func addLabel(labels []string, label string) []string {
	label = strings.TrimSpace(label)
	for _, l := range labels {
		if l == label {
			return labels
		}
	}
	return append(labels, label)
}

func removeLabel(labels []string, label string) []string {
	label = strings.TrimSpace(label)
	out := labels[:0:0]
	for _, l := range labels {
		if l != label {
			out = append(out, l)
		}
	}
	return out
}

// GetThreadTimeline returns every message in a conversation thread ordered
// oldest-first for an interactive timeline view (§P2 대화형 타임라인). It reads
// the thread directly from the store so no message is dropped by a paging
// window.
func (a *App) GetThreadTimeline(ctx context.Context, threadID string) ([]MessageView, error) {
	if threadID == "" {
		return nil, userErrf("threadID is required")
	}
	userID := userIDFrom(ctx)

	msgs, err := a.Store.GetThreadMessages(ctx, userID, threadID)
	if err != nil {
		return nil, err
	}
	// GetThreadMessages already orders by date ASC.
	timeline := make([]MessageView, 0, len(msgs))
	for i := range msgs {
		m := msgs[i]
		mv := MessageView{Message: m}
		mv.Body = a.loadBody(ctx, userID, m.ID)
		a.prepareReceivedBody(ctx, m, mv.Body, false)
		if atts, err := a.Store.ListAttachments(ctx, userID, m.ID); err == nil {
			mv.Attachments = atts
		}
		timeline = append(timeline, mv)
	}
	return timeline, nil
}
