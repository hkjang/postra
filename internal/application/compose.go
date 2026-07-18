package application

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

type CreateDraftInput struct {
	AccountID        string   `json:"account_id"`
	Kind             string   `json:"kind"` // new | reply | reply_all | forward
	ReplyToMessageID string   `json:"reply_to_message_id,omitempty"`
	To               []string `json:"to,omitempty"`
	Cc               []string `json:"cc,omitempty"`
	Subject          string   `json:"subject,omitempty"`
	Body             string   `json:"body,omitempty"`
	// Instructions, when set, asks the AI to write the draft body
	// (the result is stored as an AI-authored version; sending still
	// requires explicit user approval — §1 발송 원칙).
	Instructions string `json:"instructions,omitempty"`
}

type DraftView struct {
	Draft   domain.Draft        `json:"draft"`
	Version domain.DraftVersion `json:"version"`
}

func parseAddressStrings(in []string) ([]domain.Address, error) {
	var out []domain.Address
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		a, err := mail.ParseAddress(s)
		if err != nil {
			return nil, userErrf("invalid address %q: %v", s, err)
		}
		out = append(out, domain.Address{Name: a.Name, Email: a.Address})
	}
	return out, nil
}

func (a *App) CreateDraft(ctx context.Context, in CreateDraftInput) (*DraftView, error) {
	acc, err := a.Store.GetAccount(ctx, DefaultUserID, in.AccountID)
	if err != nil {
		return nil, err
	}
	kind := domain.DraftKind(in.Kind)
	switch kind {
	case "", domain.DraftNew:
		kind = domain.DraftNew
	case domain.DraftReply, domain.DraftReplyAll, domain.DraftForward:
		if in.ReplyToMessageID == "" {
			return nil, userErrf("%s draft requires reply_to_message_id", kind)
		}
	default:
		return nil, userErrf("invalid draft kind %q", in.Kind)
	}

	v := domain.DraftVersion{Subject: in.Subject, BodyText: in.Body, Author: "user"}
	if v.To, err = parseAddressStrings(in.To); err != nil {
		return nil, err
	}
	if v.Cc, err = parseAddressStrings(in.Cc); err != nil {
		return nil, err
	}

	var original *domain.Message
	var originalBody string
	if in.ReplyToMessageID != "" {
		mv, err := a.GetMessage(ctx, in.ReplyToMessageID, true)
		if err != nil {
			return nil, err
		}
		original = &mv.Message
		if mv.Body != nil {
			originalBody = mv.Body.TextBody
		}
		if v.Subject == "" {
			prefix := "Re: "
			if kind == domain.DraftForward {
				prefix = "Fwd: "
			}
			v.Subject = prefix + strings.TrimSpace(original.Subject)
		}
		if len(v.To) == 0 && kind != domain.DraftForward {
			// DRAFT-001: reply targets Reply-To, falling back to From.
			targets := original.ReplyTo
			if len(targets) == 0 {
				targets = []domain.Address{original.From}
			}
			v.To = targets
			if kind == domain.DraftReplyAll {
				v.Cc = append(v.Cc, dedupAddrs(append(original.To, original.Cc...), acc.Email)...)
			}
		}
	}

	if in.Instructions != "" {
		gen, err := a.generateDraftBody(ctx, in.Instructions, original, originalBody)
		if err != nil {
			return nil, err
		}
		if gen.Subject != "" && v.Subject == "" {
			v.Subject = gen.Subject
		}
		v.BodyText = gen.Body
		v.Author = "ai"
	} else if kind == domain.DraftReply || kind == domain.DraftReplyAll {
		if v.BodyText == "" {
			v.BodyText = "\n\n" + quote(originalBody)
		}
	} else if kind == domain.DraftForward && v.BodyText == "" {
		v.BodyText = "\n\n---------- Forwarded message ----------\n" + originalBody
	}

	d := &domain.Draft{
		ID: persistence.NewID("drf"), UserID: DefaultUserID, AccountID: in.AccountID,
		Kind: kind, ReplyToMessageID: in.ReplyToMessageID, Status: domain.DraftOpen,
	}
	if err := a.Store.CreateDraft(ctx, d, &v); err != nil {
		return nil, err
	}
	a.audit(ctx, "draft_create", "draft:"+d.ID, "ok", fmt.Sprintf("kind=%s author=%s", kind, v.Author))
	return &DraftView{Draft: *d, Version: v}, nil
}

type generatedDraft struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (a *App) generateDraftBody(ctx context.Context, instructions string, original *domain.Message, originalBody string) (*generatedDraft, error) {
	untrusted := ""
	targetID := "new"
	if original != nil {
		untrusted = fmt.Sprintf("Subject: %s\nFrom: %s <%s>\n\n%s",
			original.Subject, original.From.Name, original.From.Email, originalBody)
		targetID = original.ID
	}
	an, err := a.runAnalysis(ctx, "draft_reply", "message", targetID, instructions, untrusted)
	if err != nil {
		return nil, err
	}
	var g generatedDraft
	if err := json.Unmarshal([]byte(an.ResultJSON), &g); err != nil {
		return nil, fmt.Errorf("draft generation returned unexpected schema: %w", err)
	}
	if strings.TrimSpace(g.Body) == "" {
		return nil, fmt.Errorf("draft generation produced an empty body")
	}
	return &g, nil
}

type UpdateDraftInput struct {
	DraftID string   `json:"draft_id"`
	Subject *string  `json:"subject,omitempty"`
	Body    *string  `json:"body,omitempty"`
	To      []string `json:"to,omitempty"`
	Cc      []string `json:"cc,omitempty"`
	Bcc     []string `json:"bcc,omitempty"`
}

// UpdateDraft records a user-authored version on top of the current one
// (DRAFT-002/003); approvals issued for earlier versions stop matching.
func (a *App) UpdateDraft(ctx context.Context, in UpdateDraftInput) (*DraftView, error) {
	d, cur, err := a.Store.GetDraft(ctx, DefaultUserID, in.DraftID)
	if err != nil {
		return nil, err
	}
	if d.Status != domain.DraftOpen {
		return nil, userErrf("draft %s is %s and cannot be edited", d.ID, d.Status)
	}
	v := *cur
	v.Author = "user"
	if in.Subject != nil {
		v.Subject = *in.Subject
	}
	if in.Body != nil {
		v.BodyText = *in.Body
	}
	if in.To != nil {
		if v.To, err = parseAddressStrings(in.To); err != nil {
			return nil, err
		}
	}
	if in.Cc != nil {
		if v.Cc, err = parseAddressStrings(in.Cc); err != nil {
			return nil, err
		}
	}
	if in.Bcc != nil {
		if v.Bcc, err = parseAddressStrings(in.Bcc); err != nil {
			return nil, err
		}
	}
	newVer, err := a.Store.AddDraftVersion(ctx, DefaultUserID, d.ID, &v)
	if err != nil {
		return nil, err
	}
	d.CurrentVersion = newVer
	a.audit(ctx, "draft_update", "draft:"+d.ID, "ok", fmt.Sprintf("version=%d", newVer))
	return &DraftView{Draft: *d, Version: v}, nil
}

// RewriteDraft produces an AI-restyled version (mail_draft_rewrite).
func (a *App) RewriteDraft(ctx context.Context, draftID, style string) (*DraftView, error) {
	d, cur, err := a.Store.GetDraft(ctx, DefaultUserID, draftID)
	if err != nil {
		return nil, err
	}
	if d.Status != domain.DraftOpen {
		return nil, userErrf("draft %s is %s and cannot be edited", d.ID, d.Status)
	}
	untrusted := "Subject: " + cur.Subject + "\n\n" + cur.BodyText
	an, err := a.runAnalysis(ctx, "rewrite", "draft", draftID, "Rewrite style: "+style, untrusted)
	if err != nil {
		return nil, err
	}
	var g generatedDraft
	if err := json.Unmarshal([]byte(an.ResultJSON), &g); err != nil {
		return nil, fmt.Errorf("rewrite returned unexpected schema: %w", err)
	}
	v := *cur
	v.Author = "ai"
	if g.Subject != "" {
		v.Subject = g.Subject
	}
	if strings.TrimSpace(g.Body) != "" {
		v.BodyText = g.Body
	}
	newVer, err := a.Store.AddDraftVersion(ctx, DefaultUserID, d.ID, &v)
	if err != nil {
		return nil, err
	}
	d.CurrentVersion = newVer
	a.audit(ctx, "draft_rewrite", "draft:"+d.ID, "ok", style)
	return &DraftView{Draft: *d, Version: v}, nil
}

func (a *App) GetDraft(ctx context.Context, draftID string) (*DraftView, error) {
	d, v, err := a.Store.GetDraft(ctx, DefaultUserID, draftID)
	if err != nil {
		return nil, err
	}
	return &DraftView{Draft: *d, Version: *v}, nil
}

func quote(body string) string {
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

func dedupAddrs(in []domain.Address, excludeEmail string) []domain.Address {
	seen := map[string]bool{strings.ToLower(excludeEmail): true}
	var out []domain.Address
	for _, a := range in {
		k := strings.ToLower(a.Email)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out
}
