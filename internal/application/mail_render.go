package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"postra/internal/domain"
	"postra/internal/mailrender"
	"postra/internal/platform/mailhtml"
)

// RenderMailInput is shared by REST, MCP and compose. Formatting is local and
// deterministic unless SmartFormat is explicitly requested by the caller.
type RenderMailInput struct {
	AccountID        string `json:"account_id,omitempty"`
	Body             string `json:"body,omitempty"`
	BodyHTML         string `json:"body_html,omitempty"`
	Format           string `json:"format,omitempty"`
	Template         string `json:"template,omitempty"`
	UseSignature     *bool  `json:"use_signature,omitempty"`
	SignatureID      string `json:"signature_id,omitempty"`
	SmartFormat      bool   `json:"smart_format,omitempty"`
	Intent           string `json:"intent,omitempty"`
	Tone             string `json:"tone,omitempty"`
	Length           string `json:"length,omitempty"`
	Language         string `json:"language,omitempty"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	selectionOnly    bool
}

func (a *App) RenderMail(ctx context.Context, in RenderMailInput) (*mailrender.Output, error) {
	if err := a.checkComposeMCPScopes(ctx, "mail.draft"); err != nil {
		return nil, err
	}
	// Read values and lock metadata in one snapshot, so an explicit request
	// cannot override an administrator's forced preference.
	view, err := a.PersonalSettings(ctx, in.AccountID)
	if err != nil {
		return nil, err
	}
	prefs, locked := map[string]string{}, map[string]bool{}
	for _, field := range view.Fields {
		prefs[field.Key], locked[field.Key] = field.Value, field.Locked
	}
	first := func(value, key, fallback string) string {
		if value != "" && !locked[key] {
			return value
		}
		if prefs[key] != "" {
			return prefs[key]
		}
		return fallback
	}
	format := first(in.Format, "compose.format", "auto")
	template := first(in.Template, "compose.template", "clean")
	if in.selectionOnly {
		format, template = "text", "plain"
	}
	var warnings []string
	input := mailrender.Input{Body: in.Body, BodyHTML: in.BodyHTML, Format: format, Template: template, StripSignature: in.UseSignature != nil || in.SignatureID != "", ImagePolicy: a.Setting("mail.outbound_images"), ImageProxyURL: a.Setting("mail.image_proxy_url")}
	var original *domain.Message
	if in.ReplyToMessageID != "" {
		message, err := a.GetMessage(ctx, in.ReplyToMessageID, false)
		if err != nil {
			return nil, err
		}
		original = &message.Message
	}
	useSignature := prefs["compose.use_signature"] != "false"
	if in.UseSignature != nil && !locked["compose.use_signature"] {
		useSignature = *in.UseSignature
	}
	if prefs["compose.signature_policy"] == "none" {
		useSignature = false
		input.StripSignature = true
	}
	if in.selectionOnly {
		useSignature = false
	}
	signatureID := first(in.SignatureID, "compose.signature_id", "")
	if useSignature && signatureID != "" {
		signature, err := a.GetMailSignature(ctx, signatureID)
		if errors.Is(err, domain.ErrNotFound) && in.SignatureID == "" {
			warnings = append(warnings, "The default signature no longer exists; select another signature in personal settings")
		} else if err != nil {
			return nil, err
		}
		if signature != nil {
			if signature.AccountID != "" && signature.AccountID != in.AccountID {
				return nil, userErrf("signature is restricted to another mail account")
			}
			input.SignatureID, input.SignatureHTML, input.SignatureText = signature.ID, signature.BodyHTML, signature.BodyText
			if prefs["compose.signature_policy"] == "smart" && original != nil {
				input.StripSignature = true
				input.SignatureHTML = ""
				if a.hasSentReplyInThread(ctx, original) {
					input.SignatureText = ""
				} else {
					var lines []string
					for _, value := range []string{signature.DisplayName, signature.Company, signature.Email} {
						if strings.TrimSpace(value) != "" {
							lines = append(lines, value)
						}
					}
					if len(lines) == 0 {
						lines = strings.Split(signature.BodyText, "\n")
						if len(lines) > 3 {
							lines = lines[:3]
						}
					}
					input.SignatureText = strings.Join(lines, "\n")
				}
			}
		}
	}
	if in.SmartFormat {
		if err := a.checkComposeMCPScopes(ctx, "mail.ai"); err != nil {
			return nil, err
		}
		if input.StripSignature || input.SignatureHTML != "" || input.SignatureText != "" {
			if input.BodyHTML != "" {
				input.BodyHTML = mailrender.StripSignatureHTML(input.BodyHTML)
			} else if input.Format == "html" {
				input.Body = mailrender.StripSignatureHTML(input.Body)
			}
		}
		fragment, err := mailrender.Fragment(input)
		if err != nil {
			return nil, userErrf("invalid mail: %v", err)
		}
		if utf8.RuneCountInString(fragment.BodyText) > maxAIBodyChars {
			return nil, userErrf("AI formatting supports at most %d characters; select a smaller section instead", maxAIBodyChars)
		}
		inlineImages := mailhtml.ImageMarkup(fragment.BodyHTML)
		// Existing policy, masking, model routing, caching and auditing remain
		// authoritative. The provider returns structured text, never trusted HTML.
		instruction, err := writingInstruction(view, "Preserve all facts and recipients. Improve structure; return the rewritten body as Markdown in the existing JSON body field.", in.Intent, in.Tone, in.Length, in.Language)
		if err != nil {
			return nil, err
		}
		an, err := a.runAnalysis(ctx, "rewrite", "mail", "render", a.withWritingGuide(instruction), fragment.BodyText)
		if err != nil {
			return nil, err
		}
		var generated generatedDraft
		if err := json.Unmarshal([]byte(an.ResultJSON), &generated); err != nil || strings.TrimSpace(generated.Body) == "" {
			return nil, userErrf("AI formatting returned an empty or invalid body")
		}
		input.Body, input.BodyHTML, input.Format = generated.Body, "", "markdown"
		if inlineImages != "" {
			rewritten, err := mailrender.Fragment(mailrender.Input{Body: generated.Body, Format: "markdown"})
			if err != nil {
				return nil, userErrf("invalid rewritten mail")
			}
			input.BodyHTML, input.Format = rewritten.BodyHTML+inlineImages, "html"
		}
	}
	if !a.SettingBool("mail.html_enabled") {
		input.Template = "plain"
	}
	out, err := mailrender.Render(input)
	if err != nil {
		return nil, userErrf("invalid mail rendering: %v", err)
	}
	if input.Template != template {
		out.Warnings = append(out.Warnings, "HTML mail is disabled by administrator policy; rendered as plain text")
	}
	out.Warnings = append(out.Warnings, warnings...)
	return &out, nil
}

type RewriteMailTextInput struct {
	AccountID   string `json:"account_id,omitempty"`
	Text        string `json:"text"`
	Instruction string `json:"instruction,omitempty"`
	Tone        string `json:"tone,omitempty"`
	Length      string `json:"length,omitempty"`
	Language    string `json:"language,omitempty"`
}

// RewriteMailText returns a proposal only. The caller must explicitly apply it
// to a versioned draft; signature policy applies to the complete mail, never a
// selected fragment. It cannot approve or send anything.
func (a *App) RewriteMailText(ctx context.Context, in RewriteMailTextInput) (string, error) {
	if strings.TrimSpace(in.Text) == "" {
		return "", userErrf("selected text is empty")
	}
	out, err := a.RenderMail(ctx, RenderMailInput{AccountID: in.AccountID, Body: in.Text, Format: "text", Template: "plain", SmartFormat: true, Intent: in.Instruction, Tone: in.Tone, Length: in.Length, Language: in.Language, selectionOnly: true})
	if err != nil {
		return "", err
	}
	return out.BodyText, nil
}

// A bounded scan can prove a prior sent reply, never the absence of one.
// Incomplete history conservatively keeps a compact first-reply signature.
func (a *App) hasSentReplyInThread(ctx context.Context, original *domain.Message) bool {
	rows, err := a.Store.ListOutbound(ctx, userIDFrom(ctx), 500)
	if err != nil {
		return false
	}
	for _, row := range rows {
		if row.Status != domain.OutboundSent {
			continue
		}
		draft, _, err := a.Store.GetDraft(ctx, userIDFrom(ctx), row.DraftID)
		if err != nil || (draft.Kind != domain.DraftReply && draft.Kind != domain.DraftReplyAll) || draft.ReplyToMessageID == "" {
			continue
		}
		if draft.ReplyToMessageID == original.ID {
			return true
		}
		if original.ThreadID == "" {
			continue
		}
		message, err := a.Store.GetMessage(ctx, userIDFrom(ctx), draft.ReplyToMessageID)
		if err == nil && message.ThreadID == original.ThreadID {
			return true
		}
	}
	return false
}

func renderRequested(format, template, signatureID, intent, tone, length, language string, useSignature *bool, smart bool) bool {
	return format != "" || template != "" || signatureID != "" || intent != "" || tone != "" || length != "" || language != "" || useSignature != nil || smart
}

func (a *App) checkComposeMCPScopes(ctx context.Context, scopes ...string) error {
	if p, ok := PrincipalFrom(ctx); ok && p.IsMCPScoped() {
		return a.CheckMCPPermissionScopes(ctx, scopes...)
	}
	return nil
}

// Rendering policy also applies to legacy clients that do not request a theme.
func (a *App) enforceHTMLPolicy(text, html string) (string, string) {
	if html != "" && !a.SettingBool("mail.html_enabled") {
		return mailhtml.PlainText(html), ""
	}
	return text, html
}

func (a *App) sanitizeMailHTML(raw string) string {
	return mailhtml.SanitizeWithImages(raw, a.Setting("mail.outbound_images"), a.Setting("mail.image_proxy_url"))
}
