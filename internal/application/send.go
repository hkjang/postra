package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/mailhtml"
	"postra/internal/platform/metrics"
)

// ---------- payload hash & approval (§9.2) ----------

// sendPayloadHash binds account, addresses, subject, body, and draft version
// into one digest. Any change after approval produces a different hash, so
// the stored approval no longer matches and mail_send is rejected.
func sendPayloadHash(acc *domain.MailAccount, v *domain.DraftVersion) string {
	h := sha256.New()
	bodySum := sha256.Sum256([]byte(v.BodyText + "\x00" + v.BodyHTML))
	fmt.Fprintf(h, "account=%s\nfrom=%s\nto=%s\ncc=%s\nbcc=%s\nsubject=%s\nbody=%x\nversion=%d\n",
		acc.ID, acc.Email, addrKey(v.To), addrKey(v.Cc), addrKey(v.Bcc),
		v.Subject, bodySum, v.Version)
	if len(v.Attachments) > 0 {
		raw, _ := json.Marshal(v.Attachments)
		sum := sha256.Sum256(raw)
		fmt.Fprintf(h, "attachments=%x\n", sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func addrKey(a []domain.Address) string {
	parts := make([]string, len(a))
	for i, x := range a {
		parts[i] = strings.ToLower(x.Email)
	}
	return strings.Join(parts, ",")
}

func (a *App) Issue(ctx context.Context, req domain.ApprovalRequest) (domain.ApprovalToken, error) {
	token := randomToken(32)
	tokenHash := sha256.Sum256([]byte(token))
	ttl := req.TTLSeconds
	if ttl <= 0 {
		ttl = 600
	}
	expires := time.Now().Add(time.Duration(ttl) * time.Second).Unix()
	id := persistence.NewID("apr")
	err := a.Store.InsertApproval(ctx, id, req.UserID, req.ActionType,
		req.DraftID, req.DraftVersion, req.PayloadHash, hex.EncodeToString(tokenHash[:]), req.Approver, expires)
	if err != nil {
		return domain.ApprovalToken{}, err
	}
	return domain.ApprovalToken{ID: id, Token: token, Expires: expires}, nil
}

func (a *App) VerifyAndConsume(ctx context.Context, token, payloadHash string) error {
	tokenHash := sha256.Sum256([]byte(token))
	_, _, err := a.Store.ConsumeApproval(ctx, hex.EncodeToString(tokenHash[:]), payloadHash)
	return err
}

var _ domain.ApprovalService = (*App)(nil)

// ---------- validation & preview ----------

// crlfGuard rejects header injection attempts (SMTP-003, §13).
func crlfGuard(fields ...string) error {
	for _, f := range fields {
		if strings.ContainsAny(f, "\r\n") {
			return userErrf("header field contains CR/LF characters")
		}
	}
	return nil
}

func validateDraftForSend(acc *domain.MailAccount, v *domain.DraftVersion) error {
	if len(v.To) == 0 {
		return userErrf("draft has no recipients (DRAFT-009)")
	}
	if strings.TrimSpace(v.Subject) == "" {
		return userErrf("draft has an empty subject")
	}
	bodyText, _ := draftSendBodies(v)
	if strings.TrimSpace(bodyText) == "" {
		return userErrf("draft has an empty body")
	}
	all := append(append(append([]domain.Address{}, v.To...), v.Cc...), v.Bcc...)
	for _, addr := range all {
		if _, err := mail.ParseAddress(addr.Email); err != nil {
			return userErrf("invalid recipient address %q", addr.Email)
		}
		if err := crlfGuard(addr.Email, addr.Name); err != nil {
			return err
		}
	}
	return crlfGuard(v.Subject, acc.Email)
}

type SendPreview struct {
	DraftID      string   `json:"draft_id"`
	DraftVersion int      `json:"draft_version"`
	From         string   `json:"from"`
	To           []string `json:"to"`
	Cc           []string `json:"cc,omitempty"`
	Bcc          []string `json:"bcc,omitempty"`
	Subject      string   `json:"subject"`
	Body         string   `json:"body"`
	BodyHTML     string   `json:"body_html,omitempty"`
	// ExternalDomains flags recipients outside the sender's domain (§13
	// 잘못된 수신자 발송 통제).
	ExternalDomains []string `json:"external_domains,omitempty"`
	RecipientCount  int      `json:"recipient_count"`
	// Warnings surfaces send-time cautions (e.g. many recipients, SMTP-013)
	// the user should review before approving.
	Warnings    []string `json:"warnings,omitempty"`
	PayloadHash string   `json:"payload_hash"`
	// DLPFindings lists sensitive content categories detected when the message
	// targets an external domain (§보안 DLP). DLPBlocked is true when policy is
	// "block" and findings exist — the send will be refused.
	DLPFindings []DLPFinding             `json:"dlp_findings,omitempty"`
	DLPBlocked  bool                     `json:"dlp_blocked,omitempty"`
	Attachments []domain.DraftAttachment `json:"attachments,omitempty"`
}

func (a *App) PreviewSend(ctx context.Context, draftID string) (*SendPreview, error) {
	userID := userIDFrom(ctx)
	d, v, err := a.Store.GetDraft(ctx, userID, draftID)
	if err != nil {
		return nil, err
	}
	acc, err := a.GetAccount(ctx, d.AccountID)
	if err != nil {
		return nil, err
	}
	if err := validateDraftForSend(acc, v); err != nil {
		return nil, err
	}
	if err := a.enforceSendPolicy(acc, v); err != nil {
		return nil, err
	}
	attachmentText, err := a.draftAttachmentsPolicyText(ctx, v)
	if err != nil {
		return nil, err
	}
	policyText := draftPolicyText(v) + attachmentText
	senderDomain := domainOf(acc.Email)
	extSet := map[string]bool{}
	for _, addr := range append(append(append([]domain.Address{}, v.To...), v.Cc...), v.Bcc...) {
		if dom := domainOf(addr.Email); dom != "" && dom != senderDomain {
			extSet[dom] = true
		}
	}
	var ext []string
	for d := range extSet {
		ext = append(ext, d)
	}
	sort.Strings(ext)
	recipientCount := len(v.To) + len(v.Cc) + len(v.Bcc)
	var warnings []string
	if len(mailhtml.RemoteImageSources(v.BodyHTML)) > 0 {
		warnings = append(warnings, "Remote images are included by administrator policy. The local preview blocks remote loading; recipients may load these images.")
	}
	for _, attachment := range v.Attachments {
		if !strings.HasPrefix(attachment.MIMEType, "text/") && attachment.MIMEType != "application/json" && attachment.MIMEType != "application/xml" {
			warnings = append(warnings, "Binary attachment contents are not inspected for sensitive data; review them before approving")
			break
		}
	}
	if w := a.EffectiveConfig().Send.WarnRecipients; w > 0 && recipientCount >= w {
		warnings = append(warnings, fmt.Sprintf("%d recipients — review carefully before approving", recipientCount))
	}
	if len(ext) > 0 && a.SettingBool("send.warn_external") {
		warnings = append(warnings, "recipients on external domains: "+strings.Join(ext, ", "))
	}
	// Organization writing policy: flag banned phrases regardless of recipient.
	if banned := a.checkWritingPolicy(v.Subject, policyText); len(banned) > 0 {
		warnings = append(warnings, "writing policy: banned phrase(s) present: "+strings.Join(banned, ", "))
	}
	// DLP applies only when the message leaves the organization (external
	// recipients present) and the policy is not "off".
	var dlpFindings []DLPFinding
	dlpBlocked := false
	if len(ext) > 0 && a.dlpPolicy() != "off" {
		dlpFindings = a.scanDLP(v.Subject, policyText)
		if len(dlpFindings) > 0 {
			warnings = append(warnings, "DLP: sensitive content detected in a message to external domains "+dlpSummary(dlpFindings))
			if a.dlpPolicy() == "block" {
				dlpBlocked = true
				warnings = append(warnings, "DLP policy is 'block' — this send will be refused until the flagged content is removed")
			}
		}
	}
	bodyText, bodyHTML := draftSendBodies(v)
	return &SendPreview{
		DraftID: d.ID, DraftVersion: v.Version,
		From: acc.Email, To: emails(v.To), Cc: emails(v.Cc), Bcc: emails(v.Bcc),
		Subject: v.Subject, Body: bodyText, BodyHTML: bodyHTML,
		ExternalDomains: ext, RecipientCount: recipientCount,
		Warnings: warnings, PayloadHash: sendPayloadHash(acc, v),
		DLPFindings: dlpFindings, DLPBlocked: dlpBlocked,
		Attachments: v.Attachments,
	}, nil
}

// dlpSummary renders finding categories for a human-facing warning without
// echoing any sensitive value.
func dlpSummary(findings []DLPFinding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.Term != "" {
			parts = append(parts, fmt.Sprintf("%q×%d", f.Term, f.Count))
		} else {
			parts = append(parts, fmt.Sprintf("%s×%d", f.Type, f.Count))
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// enforceDLP re-checks DLP at send time so a "block" policy cannot be bypassed
// by requesting an approval and editing around the preview.
func (a *App) enforceDLP(acc *domain.MailAccount, v *domain.DraftVersion) error {
	if a.dlpPolicy() != "block" {
		return nil
	}
	senderDomain := domainOf(acc.Email)
	external := false
	for _, addr := range append(append(append([]domain.Address{}, v.To...), v.Cc...), v.Bcc...) {
		if dom := domainOf(addr.Email); dom != "" && dom != senderDomain {
			external = true
			break
		}
	}
	if !external {
		return nil
	}
	if findings := a.scanDLP(v.Subject, draftPolicyText(v)); len(findings) > 0 {
		return userErrf("send blocked by DLP policy: sensitive content in a message to external recipients %s", dlpSummary(findings))
	}
	return nil
}

// draftSendBodies applies the same HTML-authoritative normalization at every
// send boundary, including for legacy/imported versions that bypassed compose.
// It does not modify stored fields: approval hashes still bind the saved revision.
func draftSendBodies(v *domain.DraftVersion) (bodyText, bodyHTML string) {
	if v.BodyHTML == "" {
		return v.BodyText, ""
	}
	bodyHTML = mailhtml.SanitizeApprovedOutbound(v.BodyHTML)
	return mailhtml.PlainText(bodyHTML), bodyHTML
}

// Scan the canonical transmitted content, including retained HTML metadata.
func draftPolicyText(v *domain.DraftVersion) string {
	if v.BodyHTML == "" {
		return v.BodyText
	}
	return mailhtml.PolicyText(v.BodyHTML)
}

// RequestSendApproval issues a one-time approval token for the draft's
// current version. The caller (a human, via UI/CLI) reviews the preview
// before requesting this (§9.2, acceptance #8).
func (a *App) RequestSendApproval(ctx context.Context, draftID, approver string, ttlSeconds int) (*SendPreview, *domain.ApprovalToken, error) {
	preview, err := a.PreviewSend(ctx, draftID)
	if err != nil {
		return nil, nil, err
	}
	tok, err := a.Issue(ctx, domain.ApprovalRequest{
		UserID: userIDFrom(ctx), ActionType: "mail_send",
		DraftID: draftID, DraftVersion: preview.DraftVersion,
		PayloadHash: preview.PayloadHash, TTLSeconds: ttlSeconds, Approver: approver,
	})
	if err != nil {
		return nil, nil, err
	}
	a.audit(ctx, "send_approval_issue", "draft:"+draftID, "ok",
		fmt.Sprintf("version=%d approver=%s", preview.DraftVersion, approver))
	return preview, &tok, nil
}

// ---------- send ----------

type SendInput struct {
	DraftID        string `json:"draft_id"`
	ApprovalToken  string `json:"approval_token"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

func (a *App) Send(ctx context.Context, in SendInput) (*domain.OutboundMessage, error) {
	userID := userIDFrom(ctx)
	d, v, err := a.Store.GetDraft(ctx, userID, in.DraftID)
	if err != nil {
		return nil, err
	}
	acc, err := a.GetAccount(ctx, d.AccountID)
	if err != nil {
		return nil, err
	}
	if acc.Status != domain.AccountActive {
		return nil, userErrf("account %s is %s; sending blocked", acc.ID, acc.Status)
	}
	if err := validateDraftForSend(acc, v); err != nil {
		return nil, err
	}
	if err := a.enforceSendPolicy(acc, v); err != nil {
		return nil, err
	}
	// DLP block enforced at the actual send boundary (§보안 DLP).
	if err := a.enforceDraftDLP(ctx, acc, v); err != nil {
		a.audit(ctx, "mail_send", "draft:"+d.ID, "denied", err.Error())
		return nil, err
	}

	// SMTP-007: idempotent replay returns the original outcome (no new send,
	// so it must not be counted against the rate limit).
	idemKey := in.IdempotencyKey
	if idemKey == "" {
		idemKey = fmt.Sprintf("draft:%s:v%d", d.ID, v.Version)
	}
	if existing, err := a.Store.GetOutboundByIdemKey(ctx, userID, idemKey); err == nil {
		return safeOutbound(existing), nil
	}

	// SMTP-012: rolling-window send quota per account.
	if err := a.checkSendRate(ctx, acc.ID); err != nil {
		a.audit(ctx, "mail_send", "draft:"+d.ID, "denied", err.Error())
		return nil, err
	}

	// Approval token must match the *current* payload (§9.2): recompute and
	// consume atomically. AI or MCP callers cannot skip this — there is no
	// other send path.
	payloadHash := sendPayloadHash(acc, v)
	if err := a.VerifyAndConsume(ctx, in.ApprovalToken, payloadHash); err != nil {
		a.audit(ctx, "mail_send", "draft:"+d.ID, "denied", err.Error())
		return nil, userErrf("send rejected: %v", err)
	}

	msgID := fmt.Sprintf("<%s@postra.local>", randomToken(16))
	out := &domain.OutboundMessage{
		ID: persistence.NewID("out"), UserID: userID,
		DraftID: d.ID, DraftVersion: v.Version, IdempotencyKey: idemKey,
		MessageID: msgID, Status: domain.OutboundQueued,
	}
	if err := a.Store.CreateOutbound(ctx, out); err != nil {
		return nil, err
	}

	receipt, sendErr := a.deliver(ctx, out, acc, v)
	return a.applySendResult(ctx, out, d.ID, receipt, sendErr), nil
}

// deliver builds the RFC822 message and hands it to SMTP. A build error is a
// plain (permanent) error; SMTP errors carry temporary/permanent
// classification from the adapter.
func (a *App) deliver(ctx context.Context, out *domain.OutboundMessage, acc *domain.MailAccount, v *domain.DraftVersion) (domain.SendReceipt, error) {
	// Scheduled retries also pass this gate. A policy tightened after approval
	// must never be bypassed, nor may we silently change an approved HTML body.
	if err := a.enforceSendPolicy(acc, v); err != nil {
		return domain.SendReceipt{}, err
	}
	if err := a.enforceDLP(acc, v); err != nil {
		return domain.SendReceipt{}, err
	}
	attachmentData := map[string][]byte{}
	policyVersion := *v
	policyVersion.BodyText, policyVersion.BodyHTML = draftPolicyText(v), ""
	for _, attachment := range v.Attachments {
		data, err := a.readDraftAttachment(ctx, attachment)
		if err != nil {
			return domain.SendReceipt{}, err
		}
		attachmentData[attachment.ID] = data
		policyVersion.BodyText += "\n" + attachmentPolicyText(attachment, data)
	}
	if err := a.enforceDLP(acc, &policyVersion); err != nil {
		return domain.SendReceipt{}, err
	}
	var inReplyTo, references string
	if orig, err := a.replyContext(ctx, out.DraftID); err == nil && orig != nil && orig.MessageID != "" {
		inReplyTo = orig.MessageID
		references = strings.TrimSpace(orig.References + " " + orig.MessageID)
	}
	var raw []byte
	var err error
	if len(v.Attachments) > 0 {
		raw, err = buildMIMEWithAttachments(acc, v, out.MessageID, inReplyTo, references, attachmentData)
	} else {
		raw, err = buildMIME(acc, v, out.MessageID, inReplyTo, references)
	}
	if err != nil {
		return domain.SendReceipt{}, err // permanent
	}
	var secret *domain.SecretHandle
	if acc.SMTPSecret != "" && acc.SMTPAuth != "none" {
		secret, err = a.Secrets.Acquire(ctx, acc.SMTPSecret, domain.PurposeSMTPAuth)
		if err != nil {
			return domain.SendReceipt{}, err // permanent (secret missing/revoked)
		}
		a.Store.TouchCredential(ctx, acc.SMTPSecret)
	}
	rcpts := append(append(emails(v.To), emails(v.Cc)...), emails(v.Bcc)...)
	return a.SMTP.Send(ctx, domain.SMTPSendOptions{
		Host: acc.SMTPHost, Port: acc.SMTPPort, Security: acc.SMTPSecurity,
		AuthMethod: acc.SMTPAuth, Username: acc.SMTPUsername, Password: secret,
		InsecureSkipVerify: acc.InsecureSkipVerify,
		ConnectTimeoutSec:  a.EffectiveConfig().Sync.ConnectTimeoutSec,
		CommandTimeoutSec:  a.EffectiveConfig().Sync.CommandTimeoutSec,
	}, domain.Envelope{From: acc.Email, To: rcpts}, bytes.NewReader(raw))
}

func (a *App) replyContext(ctx context.Context, draftID string) (*domain.Message, error) {
	d, _, err := a.Store.GetDraft(ctx, userIDFrom(ctx), draftID)
	if err != nil || d.ReplyToMessageID == "" {
		return nil, err
	}
	return a.Store.GetMessage(ctx, userIDFrom(ctx), d.ReplyToMessageID)
}

// applySendResult records the outcome of a delivery attempt and, for
// temporary failures within the retry budget, schedules a backoff retry
// (SMTP-010/011). Permanent failures and exhausted retries end in 'failed';
// an uncertain result is never auto-retried (SMTP-008/009).
func (a *App) applySendResult(ctx context.Context, out *domain.OutboundMessage, draftID string, receipt domain.SendReceipt, sendErr error) *domain.OutboundMessage {
	attempts := out.Attempts + 1
	out.Attempts = attempts
	maxRetries := a.EffectiveConfig().Send.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 1 // no retries configured: single attempt
	}
	switch {
	case sendErr != nil && isTemporary(sendErr) && attempts < maxRetries:
		next := time.Now().Add(a.retryBackoff(attempts)).Unix()
		message := outboundDiagnostic(domain.OutboundRetryWait)
		_ = a.Store.MarkOutboundRetry(ctx, out.ID, message, attempts, next)
		out.Status, out.SMTPResponse, out.NextAttemptAt = domain.OutboundRetryWait, message, next
		a.audit(ctx, "mail_send", "draft:"+draftID, "retry_scheduled",
			fmt.Sprintf("attempt=%d next=%d", attempts, next))
	case sendErr != nil && isTemporary(sendErr):
		message := outboundDiagnostic(domain.OutboundFailed)
		_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundFailed, message, attempts)
		out.Status, out.SMTPResponse = domain.OutboundFailed, message
		a.audit(ctx, "mail_send", "draft:"+draftID, "failed", "retries exhausted")
	case sendErr != nil:
		message := outboundDiagnostic(domain.OutboundFailed)
		_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundFailed, message, attempts)
		out.Status, out.SMTPResponse = domain.OutboundFailed, message
		a.audit(ctx, "mail_send", "draft:"+draftID, "error", message)
	case receipt.Uncertain:
		message := outboundDiagnostic(domain.OutboundUncertain)
		_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundUncertain, message, attempts)
		out.Status, out.SMTPResponse = domain.OutboundUncertain, message
		a.audit(ctx, "mail_send", "draft:"+draftID, "uncertain", "response lost after DATA")
	default:
		message := outboundDiagnostic(domain.OutboundSent)
		_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundSent, message, attempts)
		_ = a.Store.SetDraftStatus(ctx, userIDFrom(ctx), draftID, domain.DraftSent)
		out.Status, out.SMTPResponse = domain.OutboundSent, message
		a.audit(ctx, "mail_send", "draft:"+draftID, "ok", "outbound="+out.ID)
	}
	metrics.SMTPSend.WithLabelValues(sendResultLabel(out.Status)).Inc()
	return out
}

// sendResultLabel maps a terminal/interim outbound status to a stable,
// low-cardinality Prometheus label.
func sendResultLabel(s domain.OutboundStatus) string {
	switch s {
	case domain.OutboundSent:
		return "sent"
	case domain.OutboundRetryWait:
		return "deferred"
	case domain.OutboundUncertain:
		return "uncertain"
	case domain.OutboundFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// retryBackoff is exponential (base * 2^(attempt-1)) capped at RetryMaxSeconds.
func (a *App) retryBackoff(attempt int) time.Duration {
	cfg := a.EffectiveConfig()
	base := time.Duration(cfg.Send.RetryBaseSeconds) * time.Second
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	if max := time.Duration(cfg.Send.RetryMaxSeconds) * time.Second; max > 0 && d > max {
		d = max
	}
	return d
}

func isTemporary(err error) bool {
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

// OutboundView pairs a delivery record with enough of its draft to be
// recognisable. An ID alone tells the sender nothing about which mail is stuck.
type OutboundView struct {
	Outbound domain.OutboundMessage `json:"outbound"`
	Subject  string                 `json:"subject,omitempty"`
	To       []string               `json:"to,omitempty"`
}

// ListOutbound reports recent sends and their outcome. A temporary SMTP
// failure parks a message in the retry queue, which until now was visible only
// to the worker — the sender saw a success page and never learned otherwise.
func (a *App) ListOutbound(ctx context.Context, limit int) ([]OutboundView, error) {
	userID := userIDFrom(ctx)
	rows, err := a.Store.ListOutbound(ctx, userID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]OutboundView, 0, len(rows))
	for _, o := range rows {
		view := OutboundView{Outbound: *safeOutbound(&o)}
		// Best-effort enrichment: a discarded draft must not hide its delivery.
		if _, v, derr := a.Store.GetDraft(ctx, userID, o.DraftID); derr == nil && v != nil {
			view.Subject = v.Subject
			for _, addr := range v.To {
				view.To = append(view.To, addr.Email)
			}
		}
		out = append(out, view)
	}
	return out, nil
}

func (a *App) GetOutbound(ctx context.Context, id string) (*domain.OutboundMessage, error) {
	out, err := a.Store.GetOutbound(ctx, userIDFrom(ctx), id)
	return safeOutbound(out), err
}

// checkSendRate enforces per-account per-minute and per-hour send quotas
// against the durable outbound history (SMTP-012).
func (a *App) checkSendRate(ctx context.Context, accountID string) error {
	now := time.Now()
	cfg := a.EffectiveConfig()
	if lim := cfg.Send.MaxPerMinute; lim > 0 {
		n, err := a.Store.CountSentSince(ctx, userIDFrom(ctx), accountID, now.Add(-time.Minute).Unix())
		if err != nil {
			return err
		}
		if n >= lim {
			return userErrf("send rate limit reached: %d/min for this account", lim)
		}
	}
	if lim := cfg.Send.MaxPerHour; lim > 0 {
		n, err := a.Store.CountSentSince(ctx, userIDFrom(ctx), accountID, now.Add(-time.Hour).Unix())
		if err != nil {
			return err
		}
		if n >= lim {
			return userErrf("send rate limit reached: %d/hour for this account", lim)
		}
	}
	return nil
}

func (a *App) enforceSendPolicy(acc *domain.MailAccount, v *domain.DraftVersion) error {
	for _, source := range mailhtml.RemoteImageSources(mailhtml.SanitizeApprovedOutbound(v.BodyHTML)) {
		mode := a.Setting("mail.outbound_images")
		if mode == "allow" {
			continue
		}
		if mode == "proxy" && mailhtml.IsProxiedImage(source, a.Setting("mail.image_proxy_url")) {
			continue
		}
		return userErrf("outbound image policy changed; render the draft again and request a new approval")
	}
	if err := a.validateDraftAttachments(v); err != nil {
		return err
	}
	if !a.SettingBool("mail.smtp_enabled") {
		return userErrf("SMTP sending is disabled by administrator policy")
	}
	if a.SettingBool("mail.tls_required") && (acc.SMTPSecurity != "tls" && acc.SMTPSecurity != "starttls" || acc.InsecureSkipVerify) {
		return userErrf("verified SMTP TLS is required by administrator policy")
	}
	if a.SettingBool("mail.smtp_auth_required") && (acc.SMTPAuth == "none" || acc.SMTPAuth == "" || acc.SMTPSecret == "" || acc.SMTPUsername == "") {
		return userErrf("SMTP authentication is required by administrator policy")
	}
	if !a.SettingBool("mail.html_enabled") && v.BodyHTML != "" {
		return userErrf("HTML mail is disabled by administrator policy; save as plain text and request approval again")
	}
	if max := a.SettingInt("send.max_recipients"); max > 0 && len(v.To)+len(v.Cc)+len(v.Bcc) > max {
		return userErrf("recipient count exceeds administrator limit (%d)", max)
	}
	return nil
}

// ---------- MIME construction ----------

func formatAddrList(list []domain.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		addr := mail.Address{Name: a.Name, Address: a.Email}
		parts[i] = addr.String()
	}
	return strings.Join(parts, ", ")
}

// buildMIME renders the outgoing RFC822 message. Envelope recipients are
// handled separately in Send (SMTP-004); Bcc never appears in headers.
func buildMIME(acc *domain.MailAccount, v *domain.DraftVersion, msgID, inReplyTo, references string) ([]byte, error) {
	bodyText, bodyHTML := draftSendBodies(v)
	var b bytes.Buffer
	write := func(k, val string) {
		if val != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, val)
		}
	}
	from := mail.Address{Name: acc.Name, Address: acc.Email}
	write("From", from.String())
	write("To", formatAddrList(v.To))
	write("Cc", formatAddrList(v.Cc))
	write("Subject", mime.QEncoding.Encode("utf-8", v.Subject))
	write("Date", time.Now().Format(time.RFC1123Z))
	write("Message-ID", msgID)
	write("In-Reply-To", inReplyTo)
	write("References", references)
	write("MIME-Version", "1.0")
	if bodyHTML != "" {
		parts := multipart.NewWriter(&b)
		write("Content-Type", mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": parts.Boundary()}))
		b.WriteString("\r\n")
		for _, body := range []struct{ mediaType, value string }{
			{"text/plain", bodyText}, {"text/html", bodyHTML},
		} {
			header := textproto.MIMEHeader{}
			header.Set("Content-Type", mime.FormatMediaType(body.mediaType, map[string]string{"charset": "utf-8"}))
			header.Set("Content-Transfer-Encoding", "quoted-printable")
			part, err := parts.CreatePart(header)
			if err != nil {
				return nil, err
			}
			qp := quotedprintable.NewWriter(part)
			if _, err := qp.Write([]byte(body.value)); err != nil {
				return nil, err
			}
			if err := qp.Close(); err != nil {
				return nil, err
			}
		}
		if err := parts.Close(); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	write("Content-Type", `text/plain; charset="utf-8"`)
	write("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(bodyText)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	b.WriteString("\r\n")
	return b.Bytes(), nil
}

func emails(list []domain.Address) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.Email
	}
	return out
}

func domainOf(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return ""
}
