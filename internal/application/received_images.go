package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/mail"
	"postra/internal/adapters/mailparse"
	"postra/internal/domain"
	"postra/internal/platform/receivedhtml"
	"strings"
)

func receivedImageIdentity(message domain.Message) (sender, host string) {
	parsed, err := mail.ParseAddress(message.From.Email)
	if err != nil {
		return "", ""
	}
	sender = strings.ToLower(strings.TrimSpace(parsed.Address))
	_, host, _ = strings.Cut(sender, "@")
	return sender, host
}
func imageTrustKey(userID, scope, value string) string {
	hash := sha256.Sum256([]byte(value))
	return "internal.received_images." + base64.RawURLEncoding.EncodeToString([]byte(userID)) + "." + scope + "." + hex.EncodeToString(hash[:])
}
func (a *App) prepareReceivedBody(ctx context.Context, message domain.Message, body *domain.MessageBody, once bool) {
	if body == nil || body.HTMLSanitized == "" || body.Unavailable {
		return
	}
	if !a.SettingBool("mail.html_enabled") {
		body.HTMLSanitized = ""
		return
	}
	source := body.HTMLSanitized
	// Earlier versions discarded remote URLs at ingestion. Recover bounded,
	// owned raw MIME in memory; do not rewrite the stored body or mark mail read.
	if !strings.Contains(source, receivedhtml.Marker) && message.RawURI != "" && a.Objects != nil {
		if reader, err := a.Objects.Get(message.RawURI); err == nil {
			data, readErr := io.ReadAll(io.LimitReader(reader, (5<<20)+1))
			reader.Close()
			if readErr == nil && len(data) <= 5<<20 {
				if parsed := mailparse.Parse(data); parsed.HTMLSafe != "" {
					source = parsed.HTMLSafe
				}
			}
		}
	}
	policy := a.Setting("mail.external_images")
	sender, host := receivedImageIdentity(message)
	senderTrusted, domainTrusted := false, false
	if policy == "allow_sender" || policy == "allow_domain" {
		if settings, err := a.Store.GetSettings(ctx); err == nil {
			senderTrusted = sender != "" && settings[imageTrustKey(message.UserID, "sender", sender)] == "true"
			if policy == "allow_domain" {
				domainTrusted = host != "" && settings[imageTrustKey(message.UserID, "domain", host)] == "true"
			}
		}
	}
	allowed := (policy == "allow_once" || policy == "allow_sender" || policy == "allow_domain") && (once || senderTrusted || domainTrusted)
	body.HTMLSanitized, body.ExternalImages = receivedhtml.Render(source, allowed)
	body.ImagesAllowed, body.ImagePolicy, body.ImageSenderTrusted, body.ImageDomainTrusted = allowed, policy, senderTrusted, domainTrusted
}

func (a *App) GetReceivedMessage(ctx context.Context, id string, once bool) (*MessageView, error) {
	return a.getMessage(ctx, id, true, false, once)
}

// AllowReceivedImages is a user decision for the currently owned message's
// sender, never an arbitrary supplied address. Admin block always wins.
func (a *App) AllowReceivedImages(ctx context.Context, id, scope string, revoke bool) error {
	if err := CheckMCPScopes(ctx, "mail.work"); err != nil {
		return err
	}
	message, err := a.Store.GetMessage(ctx, userIDFrom(ctx), id)
	if err != nil {
		return err
	}
	if scope != "sender" && scope != "domain" {
		return userErrf("invalid image trust scope")
	}
	policy := a.Setting("mail.external_images")
	if !revoke && ((scope == "sender" && policy != "allow_sender" && policy != "allow_domain") || (scope == "domain" && policy != "allow_domain")) {
		return &domain.PublicError{Code: "forbidden", Message: "관리자 정책에서 이 외부 이미지 허용 방식을 사용할 수 없습니다.", Status: 403}
	}
	sender, host := receivedImageIdentity(*message)
	value := sender
	if scope == "domain" {
		value = host
	}
	if value == "" {
		return userErrf("message sender is unavailable")
	}
	choice := "true"
	if revoke {
		choice = ""
	}
	if err := a.Store.UpsertSettings(ctx, map[string]string{imageTrustKey(message.UserID, scope, value): choice}); err != nil {
		return err
	}
	a.audit(ctx, "received_image_preference", "message:"+id, "ok", "scope="+scope+" revoke="+map[bool]string{true: "true", false: "false"}[revoke])
	return nil
}
