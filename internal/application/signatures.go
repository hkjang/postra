package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/mailrender"
)

type SaveMailSignatureInput struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	AccountID   string `json:"account_id,omitempty"`
	Body        string `json:"body,omitempty"`
	BodyHTML    string `json:"body_html,omitempty"`
	Format      string `json:"format,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Title       string `json:"title,omitempty"`
	Department  string `json:"department,omitempty"`
	Company     string `json:"company,omitempty"`
	Phone       string `json:"phone,omitempty"`
	Email       string `json:"email,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
}

// A separate setting per signature prevents unrelated concurrent saves from
// overwriting a whole user's collection. Base64 makes ownership prefixes
// unambiguous even for imported user IDs containing separators.
func signaturePrefix(userID string) string {
	return "internal.signatures." + base64.RawURLEncoding.EncodeToString([]byte(userID)) + "."
}

func (a *App) ListMailSignatures(ctx context.Context) ([]domain.MailSignature, error) {
	if err := a.checkComposeMCPScopes(ctx, "mail.draft"); err != nil {
		return nil, err
	}
	userID := userIDFrom(ctx)
	values, err := a.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.MailSignature, 0)
	for key, value := range values {
		if !strings.HasPrefix(key, signaturePrefix(userID)) || value == "" {
			continue
		}
		var signature domain.MailSignature
		if err := json.Unmarshal([]byte(value), &signature); err != nil {
			return nil, fmt.Errorf("stored mail signature could not be decoded")
		}
		if signature.UserID != userID || key != signaturePrefix(userID)+signature.ID {
			continue
		}
		out = append(out, signature)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (a *App) GetMailSignature(ctx context.Context, id string) (*domain.MailSignature, error) {
	if err := a.checkComposeMCPScopes(ctx, "mail.draft"); err != nil {
		return nil, err
	}
	if !validSignatureID(id) {
		return nil, domain.ErrNotFound
	}
	userID := userIDFrom(ctx)
	values, err := a.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	raw := values[signaturePrefix(userID)+id]
	if raw == "" {
		return nil, domain.ErrNotFound
	}
	var signature domain.MailSignature
	if err := json.Unmarshal([]byte(raw), &signature); err != nil {
		return nil, fmt.Errorf("stored mail signature could not be decoded")
	}
	if signature.ID != id || signature.UserID != userID {
		return nil, domain.ErrNotFound
	}
	return &signature, nil
}

func validSignatureID(id string) bool {
	if !strings.HasPrefix(id, "sig_") || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (a *App) SaveMailSignature(ctx context.Context, in SaveMailSignatureInput) (*domain.MailSignature, error) {
	if err := a.checkComposeMCPScopes(ctx, "mail.draft"); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 120 {
		return nil, userErrf("signature name must contain 1 to 120 characters")
	}
	if len(in.Body)+len(in.BodyHTML) > 64<<10 {
		return nil, userErrf("signature must not exceed 64 KiB")
	}
	for _, field := range []string{in.DisplayName, in.Title, in.Department, in.Company, in.Phone, in.Email} {
		if len([]rune(field)) > 200 || strings.ContainsAny(field, "\r\n\x00") {
			return nil, userErrf("signature identity fields must be single lines of at most 200 characters")
		}
	}
	if in.Email != "" {
		address, err := mail.ParseAddress(in.Email)
		if err != nil || address.Address != in.Email {
			return nil, userErrf("invalid signature email address")
		}
	}
	if in.LogoURL != "" {
		target, err := url.Parse(in.LogoURL)
		if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" || target.User != nil || len(in.LogoURL) > 2048 || strings.ContainsAny(in.LogoURL, "\r\n\x00") {
			return nil, userErrf("logo URL must be an HTTP(S) URL without credentials")
		}
	}
	if strings.TrimSpace(in.Body) == "" && strings.TrimSpace(in.BodyHTML) == "" {
		var lines []string
		for _, value := range []string{strings.TrimSpace(in.DisplayName + " " + in.Title), strings.TrimSpace(in.Department + " " + in.Company), in.Phone, in.Email} {
			if strings.TrimSpace(value) != "" {
				lines = append(lines, html.EscapeString(value))
			}
		}
		in.BodyHTML, in.Format = "<p>"+strings.Join(lines, "<br>")+"</p>", "html"
		if in.LogoURL != "" {
			in.BodyHTML = `<p><img src="` + html.EscapeString(in.LogoURL) + `" alt="` + html.EscapeString(in.Company) + ` 로고" style="max-width:180px;height:auto"></p>` + in.BodyHTML
		}
	}
	if in.AccountID != "" {
		if _, err := a.GetAccount(ctx, in.AccountID); err != nil {
			return nil, err
		}
	}
	fragment, err := mailrender.Fragment(mailrender.Input{Body: in.Body, BodyHTML: in.BodyHTML, Format: in.Format, ImagePolicy: a.Setting("mail.outbound_images"), ImageProxyURL: a.Setting("mail.image_proxy_url")})
	if err != nil {
		return nil, userErrf("invalid signature: %v", err)
	}
	if strings.TrimSpace(fragment.BodyText) == "" {
		return nil, userErrf("signature body is empty")
	}
	now := time.Now().Unix()
	signature := domain.MailSignature{ID: persistence.NewID("sig"), UserID: userIDFrom(ctx), Name: name, AccountID: in.AccountID, BodyHTML: fragment.BodyHTML, BodyText: fragment.BodyText, CreatedAt: now, UpdatedAt: now}
	signature.DisplayName, signature.Title, signature.Department, signature.Company, signature.Phone, signature.Email = in.DisplayName, in.Title, in.Department, in.Company, in.Phone, in.Email
	signature.LogoURL = in.LogoURL
	if in.ID != "" {
		existing, err := a.GetMailSignature(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		signature.ID, signature.CreatedAt = existing.ID, existing.CreatedAt
	}
	value, err := json.Marshal(signature)
	if err != nil {
		return nil, err
	}
	if err := a.Store.UpsertSettings(ctx, map[string]string{signaturePrefix(signature.UserID) + signature.ID: string(value)}); err != nil {
		return nil, err
	}
	a.audit(ctx, "signature_save", "signature:"+signature.ID, "ok", "")
	return &signature, nil
}

func (a *App) DeleteMailSignature(ctx context.Context, id string) error {
	signature, err := a.GetMailSignature(ctx, id)
	if err != nil {
		return err
	}
	if err := a.Store.UpsertSettings(ctx, map[string]string{signaturePrefix(userIDFrom(ctx)) + signature.ID: ""}); err != nil {
		return err
	}
	a.audit(ctx, "signature_delete", "signature:"+id, "ok", "")
	return nil
}
