package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
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
	if strings.TrimSpace(v.BodyText) == "" && strings.TrimSpace(v.BodyHTML) == "" {
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
	// ExternalDomains flags recipients outside the sender's domain (§13
	// 잘못된 수신자 발송 통제).
	ExternalDomains []string `json:"external_domains,omitempty"`
	RecipientCount  int      `json:"recipient_count"`
	// Warnings surfaces send-time cautions (e.g. many recipients, SMTP-013)
	// the user should review before approving.
	Warnings    []string `json:"warnings,omitempty"`
	PayloadHash string   `json:"payload_hash"`
}

func (a *App) PreviewSend(ctx context.Context, draftID string) (*SendPreview, error) {
	d, v, err := a.Store.GetDraft(ctx, DefaultUserID, draftID)
	if err != nil {
		return nil, err
	}
	acc, err := a.Store.GetAccount(ctx, DefaultUserID, d.AccountID)
	if err != nil {
		return nil, err
	}
	if err := validateDraftForSend(acc, v); err != nil {
		return nil, err
	}
	senderDomain := domainOf(acc.Email)
	extSet := map[string]bool{}
	for _, addr := range append(append([]domain.Address{}, v.To...), v.Cc...) {
		if dom := domainOf(addr.Email); dom != "" && dom != senderDomain {
			extSet[dom] = true
		}
	}
	var ext []string
	for d := range extSet {
		ext = append(ext, d)
	}
	recipientCount := len(v.To) + len(v.Cc) + len(v.Bcc)
	var warnings []string
	if w := a.Cfg.Send.WarnRecipients; w > 0 && recipientCount >= w {
		warnings = append(warnings, fmt.Sprintf("%d recipients — review carefully before approving", recipientCount))
	}
	if len(ext) > 0 {
		warnings = append(warnings, "recipients on external domains: "+strings.Join(ext, ", "))
	}
	return &SendPreview{
		DraftID: d.ID, DraftVersion: v.Version,
		From: acc.Email, To: emails(v.To), Cc: emails(v.Cc), Bcc: emails(v.Bcc),
		Subject: v.Subject, Body: v.BodyText,
		ExternalDomains: ext, RecipientCount: recipientCount,
		Warnings: warnings, PayloadHash: sendPayloadHash(acc, v),
	}, nil
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
		UserID: DefaultUserID, ActionType: "mail_send",
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
	d, v, err := a.Store.GetDraft(ctx, DefaultUserID, in.DraftID)
	if err != nil {
		return nil, err
	}
	acc, err := a.Store.GetAccount(ctx, DefaultUserID, d.AccountID)
	if err != nil {
		return nil, err
	}
	if acc.Status != domain.AccountActive {
		return nil, userErrf("account %s is %s; sending blocked", acc.ID, acc.Status)
	}
	if err := validateDraftForSend(acc, v); err != nil {
		return nil, err
	}

	// SMTP-007: idempotent replay returns the original outcome (no new send,
	// so it must not be counted against the rate limit).
	idemKey := in.IdempotencyKey
	if idemKey == "" {
		idemKey = fmt.Sprintf("draft:%s:v%d", d.ID, v.Version)
	}
	if existing, err := a.Store.GetOutboundByIdemKey(ctx, DefaultUserID, idemKey); err == nil {
		return existing, nil
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
		ID: persistence.NewID("out"), UserID: DefaultUserID,
		DraftID: d.ID, DraftVersion: v.Version, IdempotencyKey: idemKey,
		MessageID: msgID, Status: domain.OutboundQueued,
	}
	if err := a.Store.CreateOutbound(ctx, out); err != nil {
		return nil, err
	}

	var inReplyTo, references string
	if d.ReplyToMessageID != "" {
		if orig, err := a.Store.GetMessage(ctx, DefaultUserID, d.ReplyToMessageID); err == nil && orig.MessageID != "" {
			inReplyTo = orig.MessageID
			references = strings.TrimSpace(orig.References + " " + orig.MessageID)
		}
	}
	raw, err := buildMIME(acc, v, msgID, inReplyTo, references)
	if err != nil {
		_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundFailed, err.Error(), 1)
		return nil, err
	}

	var secret *domain.SecretHandle
	if acc.SMTPSecret != "" && acc.SMTPAuth != "none" {
		secret, err = a.Secrets.Acquire(ctx, acc.SMTPSecret, domain.PurposeSMTPAuth)
		if err != nil {
			_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundFailed, "secret acquisition failed", 1)
			return nil, err
		}
		a.Store.TouchCredential(ctx, acc.SMTPSecret)
	}
	rcpts := append(append(emails(v.To), emails(v.Cc)...), emails(v.Bcc)...)
	receipt, sendErr := a.SMTP.Send(ctx, domain.SMTPSendOptions{
		Host: acc.SMTPHost, Port: acc.SMTPPort, Security: acc.SMTPSecurity,
		AuthMethod: acc.SMTPAuth, Username: acc.SMTPUsername, Password: secret,
		InsecureSkipVerify: acc.InsecureSkipVerify,
		ConnectTimeoutSec:  a.Cfg.Sync.ConnectTimeoutSec,
	}, domain.Envelope{From: acc.Email, To: rcpts}, bytes.NewReader(raw))

	switch {
	case sendErr != nil:
		_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundFailed, sendErr.Error(), 1)
		out.Status, out.SMTPResponse = domain.OutboundFailed, sendErr.Error()
		a.audit(ctx, "mail_send", "draft:"+d.ID, "error", sendErr.Error())
		return out, sendErr
	case receipt.Uncertain:
		// SMTP-008/009: recorded, surfaced, never auto-retried.
		_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundUncertain, receipt.ServerResponse, 1)
		out.Status, out.SMTPResponse = domain.OutboundUncertain, receipt.ServerResponse
		a.audit(ctx, "mail_send", "draft:"+d.ID, "uncertain", "response lost after DATA")
		return out, nil
	default:
		_ = a.Store.UpdateOutbound(ctx, out.ID, domain.OutboundSent, receipt.ServerResponse, 1)
		_ = a.Store.SetDraftStatus(ctx, DefaultUserID, d.ID, domain.DraftSent)
		out.Status, out.SMTPResponse = domain.OutboundSent, receipt.ServerResponse
		a.audit(ctx, "mail_send", "draft:"+d.ID, "ok",
			fmt.Sprintf("outbound=%s rcpt=%d", out.ID, len(rcpts)))
		return out, nil
	}
}

func (a *App) GetOutbound(ctx context.Context, id string) (*domain.OutboundMessage, error) {
	return a.Store.GetOutbound(ctx, DefaultUserID, id)
}

// checkSendRate enforces per-account per-minute and per-hour send quotas
// against the durable outbound history (SMTP-012).
func (a *App) checkSendRate(ctx context.Context, accountID string) error {
	now := time.Now()
	if lim := a.Cfg.Send.MaxPerMinute; lim > 0 {
		n, err := a.Store.CountSentSince(ctx, DefaultUserID, accountID, now.Add(-time.Minute).Unix())
		if err != nil {
			return err
		}
		if n >= lim {
			return userErrf("send rate limit reached: %d/min for this account", lim)
		}
	}
	if lim := a.Cfg.Send.MaxPerHour; lim > 0 {
		n, err := a.Store.CountSentSince(ctx, DefaultUserID, accountID, now.Add(-time.Hour).Unix())
		if err != nil {
			return err
		}
		if n >= lim {
			return userErrf("send rate limit reached: %d/hour for this account", lim)
		}
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
	write("Content-Type", `text/plain; charset="utf-8"`)
	write("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(v.BodyText)); err != nil {
		return nil, err
	}
	qp.Close()
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
