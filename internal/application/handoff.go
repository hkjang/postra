package application

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/handoff"
)

// Handing a message to another in-house service (aidev HANDOFF-STANDARD.md,
// sending side, format markdown).
//
// The user asks for a claim from the message screen; the browser opens the
// receiving service with it; that service collects the document from
// /api/v1/handoff/claims/{claim} with no login — the claim is the credential.
// So it is random, short-lived, single-use and bound to one message the
// issuing user could read. Nothing here ever logs or audits the token itself.

// HandoffClaimView is the standard's answer to issuing a claim.
type HandoffClaimView struct {
	Claim       string `json:"claim"`
	Source      string `json:"source"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	ExpiresAt   string `json:"expires_at"`
}

// HandoffDocument is what a collected claim yields.
type HandoffDocument struct {
	Filename    string
	ContentType string
	Body        []byte
}

// HandoffTargets lists the services a message may be sent to: the allow list
// entries whose receiving side reads markdown. Empty — the default — means
// the message screen shows no button. A settings outage reads as empty.
func (a *App) HandoffTargets(ctx context.Context) []handoff.Target {
	values, err := a.Store.GetSettings(ctx)
	if err != nil {
		return nil
	}
	targets, err := handoff.ParseTargets(values[handoff.SettingTargets])
	if err != nil {
		return nil
	}
	return handoff.Accepting(targets, handoff.FormatMarkdown)
}

// IssueHandoffClaim mints a claim for one of the caller's messages.
// requestOrigin is the scheme://host the request arrived on, used as the
// announced source when the administrator has not pinned a public origin.
func (a *App) IssueHandoffClaim(ctx context.Context, resource, format, requestOrigin string) (*HandoffClaimView, error) {
	if strings.ToLower(strings.TrimSpace(format)) != handoff.FormatMarkdown {
		return nil, userErrf("이 서비스는 %s 형식만 보냅니다", handoff.FormatMarkdown)
	}
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return nil, userErrf("resource 가 비어 있습니다")
	}
	userID := userIDFrom(ctx)
	m, err := a.Store.GetMessage(ctx, userID, resource)
	if err != nil {
		return nil, err
	}
	doc, err := a.renderHandoff(ctx, userID, m)
	if err != nil {
		return nil, err
	}
	source, err := a.handoffSource(ctx, requestOrigin)
	if err != nil {
		return nil, err
	}
	token, digest, err := handoff.NewClaim()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	claim := &domain.HandoffClaim{
		Digest: digest, UserID: userID, MessageID: m.ID,
		Filename: doc.Filename, ContentType: doc.ContentType, Bytes: int64(len(doc.Body)),
		ExpiresAt: now.Add(handoff.ClaimTTL).Unix(),
	}
	// Expired rows are swept as new ones arrive; best effort, a claim is not
	// refused because housekeeping failed.
	_ = a.Store.SweepHandoffClaims(ctx, now.Unix())
	if err := a.Store.InsertHandoffClaim(ctx, claim); err != nil {
		return nil, err
	}
	a.audit(ctx, "handoff_claim_issued", "message:"+m.ID, "ok", "format="+handoff.FormatMarkdown+" bytes="+strconv.FormatInt(claim.Bytes, 10))
	return &HandoffClaimView{
		Claim: token, Source: source, Filename: doc.Filename, ContentType: doc.ContentType,
		Bytes: claim.Bytes, ExpiresAt: time.Unix(claim.ExpiresAt, 0).Format(time.RFC3339),
	}, nil
}

// CollectHandoffClaim spends a claim and returns the document it was issued
// for. Every way a claim can be wrong — unknown, spent, expired, malformed —
// is the same domain.ErrNotFound; the standard says not to tell them apart.
func (a *App) CollectHandoffClaim(ctx context.Context, token string) (*HandoffDocument, error) {
	if !handoff.ValidClaimToken(token) {
		return nil, domain.ErrNotFound
	}
	claim, err := a.Store.ConsumeHandoffClaim(ctx, handoff.Digest(token), time.Now().Unix())
	if err != nil {
		return nil, err
	}
	m, err := a.Store.GetMessage(ctx, claim.UserID, claim.MessageID)
	if err != nil {
		// The message went away after the claim was issued; the claim is spent
		// either way.
		return nil, domain.ErrNotFound
	}
	doc, err := a.renderHandoff(ctx, claim.UserID, m)
	if err != nil {
		return nil, domain.ErrNotFound
	}
	a.audit(ctx, "handoff_claim_collected", "message:"+m.ID, "ok", "bytes="+strconv.Itoa(len(doc.Body)))
	return doc, nil
}

// handoffSource is the origin the receiving side will fetch from and compare
// against its own allow list: the pinned public origin, or failing that the
// address this request came in on.
func (a *App) handoffSource(ctx context.Context, requestOrigin string) (string, error) {
	values, err := a.Store.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	if pinned := strings.TrimSpace(values[handoff.SettingSourceOrigin]); pinned != "" {
		return handoff.NormalizeOrigin(pinned)
	}
	source, err := handoff.NormalizeOrigin(requestOrigin)
	if err != nil {
		return "", userErrf("이 서비스의 공개 주소를 알 수 없습니다. 관리자가 시스템 설정에서 공개 주소를 지정해야 합니다")
	}
	return source, nil
}

// renderHandoff turns a message into the markdown document a claim hands
// over. It is called both when the claim is issued (to announce the size) and
// when it is collected, so the body never has to be stored a second time.
func (a *App) renderHandoff(ctx context.Context, userID string, m *domain.Message) (*HandoffDocument, error) {
	body := a.loadBody(ctx, userID, m.ID)
	if body != nil && body.Unavailable {
		return nil, userErrf("본문을 읽을 수 없어 보낼 수 없습니다: %s", body.UnavailableReason)
	}
	atts, _ := a.Store.ListAttachments(ctx, userID, m.ID)
	return &HandoffDocument{
		Filename:    handoff.Filename(m.Subject),
		ContentType: handoff.ContentType,
		Body:        []byte(messageMarkdown(m, body, atts)),
	}, nil
}

// messageMarkdown writes a message the way a person would paste it into a
// document: the subject as the title, the envelope as a short list, then the
// text body. Attachments are named, not embedded — the claim carries one
// document, and the receiving side reads markdown.
func messageMarkdown(m *domain.Message, body *domain.MessageBody, atts []domain.Attachment) string {
	var b strings.Builder
	title := strings.TrimSpace(m.Subject)
	if title == "" {
		title = "(제목 없음)"
	}
	b.WriteString("# " + title + "\n\n")
	b.WriteString("- **보낸이**: " + markdownAddress(m.From) + "\n")
	if len(m.To) > 0 {
		b.WriteString("- **받는이**: " + markdownAddresses(m.To) + "\n")
	}
	if len(m.Cc) > 0 {
		b.WriteString("- **참조**: " + markdownAddresses(m.Cc) + "\n")
	}
	if m.Date != 0 {
		b.WriteString("- **날짜**: " + time.Unix(m.Date, 0).Format("2006-01-02 15:04 -0700") + "\n")
	}
	if len(atts) > 0 {
		names := make([]string, 0, len(atts))
		for _, at := range atts {
			if at.Inline {
				continue
			}
			names = append(names, fmt.Sprintf("%s (%s)", at.Name, humanBytes(at.Size)))
		}
		if len(names) > 0 {
			b.WriteString("- **첨부**: " + strings.Join(names, ", ") + "\n")
		}
	}
	b.WriteString("\n---\n\n")
	text := ""
	if body != nil {
		text = strings.TrimRight(strings.ReplaceAll(body.TextBody, "\r\n", "\n"), "\n")
	}
	if text == "" {
		text = "(본문 없음)"
	}
	b.WriteString(text + "\n")
	return b.String()
}

func markdownAddress(a domain.Address) string {
	if a.Name != "" && a.Email != "" {
		return a.Name + " <" + a.Email + ">"
	}
	if a.Email != "" {
		return a.Email
	}
	return a.Name
}

func markdownAddresses(list []domain.Address) string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if s := markdownAddress(a); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// validateHandoffSettings refuses a save that would leave the allow list
// unreadable, so a typo never silently empties the menu.
func validateHandoffSettings(incoming map[string]string) error {
	if raw, ok := incoming[handoff.SettingTargets]; ok {
		if _, err := handoff.ParseTargets(raw); err != nil {
			return userErrf("다른 서비스로 보내기: %v", err)
		}
	}
	if origin, ok := incoming[handoff.SettingSourceOrigin]; ok && strings.TrimSpace(origin) != "" {
		if _, err := handoff.NormalizeOrigin(origin); err != nil {
			return userErrf("다른 서비스로 보내기 공개 주소: %v", err)
		}
	}
	return nil
}
