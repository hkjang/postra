// Package notifymail holds the settings and message shape for event
// notifications sent through a company SMTP relay (MAIL-STANDARD). It has no
// transport and no storage: the application layer resolves recipients,
// records every attempt and hands the finished message to the SMTP port.
//
// The setting names are the same in every internal service so an operator
// learns them once. Internal relays usually accept mail on port 25 with no
// credentials and no TLS, so that is the default; authentication and
// encryption are optional extras.
package notifymail

import (
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Event names. Each one is also the suffix of the per-event switch
// (mail.notify_<event>) an administrator can turn off.
const (
	EventSendFailed          = "send_failed"           // an outbound mail stopped: retries exhausted, rejected or uncertain
	EventAssigned            = "assigned"              // a team-inbox message was assigned to me
	EventSyncCredentialError = "sync_credential_error" // mail collection stopped because the mailbox rejected the credentials
	EventIncident            = "incident"              // a new critical system incident (admins)
	EventTest                = "test"
)

// Events lists every notification kind that has a settings switch.
var Events = []string{EventSendFailed, EventAssigned, EventSyncCredentialError, EventIncident}

const (
	KeyEnabled       = "mail.enabled"
	KeyHost          = "mail.smtp_host"
	KeyPort          = "mail.smtp_port"
	KeySecurity      = "mail.security"
	KeySkipTLSVerify = "mail.skip_tls_verify"
	KeyUsername      = "mail.username"
	KeyPassword      = "mail.password" // #nosec G101 -- setting key; the stored value is a SecretStore reference
	KeyFromAddress   = "mail.from_address"
	KeyFromName      = "mail.from_name"
	KeyBaseURL       = "mail.base_url"
	KeyTimeout       = "mail.timeout_seconds"
)

// NotifyKey is the per-event switch setting for an event.
func NotifyKey(event string) string { return "mail.notify_" + event }

// Defaults are the values a fresh installation runs with: off, and shaped
// for an unauthenticated plaintext relay on port 25.
var Defaults = map[string]string{
	KeyEnabled:       "false",
	KeyHost:          "",
	KeyPort:          "25",
	KeySecurity:      "auto",
	KeySkipTLSVerify: "false",
	KeyUsername:      "",
	KeyPassword:      "",
	KeyFromAddress:   "",
	KeyFromName:      "Postra",
	KeyBaseURL:       "",
	KeyTimeout:       "10",
}

// SecurityOptions are the accepted mail.security values.
var SecurityOptions = []string{"auto", "none", "starttls", "tls"}

func init() {
	for _, event := range Events {
		Defaults[NotifyKey(event)] = "true"
	}
}

var (
	ErrDisabled = errors.New("알림 메일이 꺼져 있습니다")
	ErrInvalid  = errors.New("알림 메일 설정이 올바르지 않습니다")
)

// Config is the relay configuration read from the settings store.
type Config struct {
	Enabled     bool
	Host        string
	Port        int
	Security    string // auto | none | starttls | tls
	SkipVerify  bool
	Username    string
	PasswordRef string // SecretStore reference, never the password itself
	FromAddress string
	FromName    string
	BaseURL     string
	Timeout     time.Duration
	Events      map[string]bool
}

// ConfigFromValues reads the configuration from a flat settings map, falling
// back to Defaults for anything missing or unparsable.
func ConfigFromValues(values map[string]string) Config {
	get := func(key string) string {
		if v, ok := values[key]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return Defaults[key]
	}
	c := Config{
		Enabled:     get(KeyEnabled) == "true",
		Host:        get(KeyHost),
		Security:    strings.ToLower(get(KeySecurity)),
		SkipVerify:  get(KeySkipTLSVerify) == "true",
		Username:    get(KeyUsername),
		PasswordRef: get(KeyPassword),
		FromAddress: get(KeyFromAddress),
		FromName:    get(KeyFromName),
		BaseURL:     get(KeyBaseURL),
		Events:      map[string]bool{},
	}
	c.Port, _ = strconv.Atoi(get(KeyPort))
	if c.Port <= 0 {
		c.Port, _ = strconv.Atoi(Defaults[KeyPort])
	}
	seconds, _ := strconv.Atoi(get(KeyTimeout))
	if seconds <= 0 {
		seconds, _ = strconv.Atoi(Defaults[KeyTimeout])
	}
	c.Timeout = time.Duration(seconds) * time.Second
	// The implicit-TLS port needs no extra configuration.
	if c.Security == "auto" && c.Port == 465 {
		c.Security = "tls"
	}
	for _, event := range Events {
		c.Events[event] = get(NotifyKey(event)) == "true"
	}
	if c.FromAddress == "" && c.Host != "" {
		c.FromAddress = "postra@" + c.Host
	}
	return c
}

// Allows reports whether an event kind should be delivered. Unknown events
// are sent, so adding a notification never needs a settings change first.
func (c Config) Allows(event string) bool {
	if enabled, known := c.Events[event]; known {
		return enabled
	}
	return true
}

// Validate reports why the relay cannot be used, so a switched-on but
// half-configured installation leaves a reason instead of silently sending
// nothing.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("%w: %s 이(가) 비어 있습니다", ErrInvalid, KeyHost)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("%w: %s 은 1~65535 이어야 합니다", ErrInvalid, KeyPort)
	}
	if _, err := mail.ParseAddress(c.FromAddress); err != nil {
		return fmt.Errorf("%w: %s 은 메일 주소여야 합니다", ErrInvalid, KeyFromAddress)
	}
	switch c.Security {
	case "auto", "none", "starttls", "tls":
	default:
		return fmt.Errorf("%w: %s 은 auto, none, starttls, tls 중 하나여야 합니다", ErrInvalid, KeySecurity)
	}
	return nil
}

// From is the RFC 5322 From header value.
func (c Config) From() string {
	from := strings.TrimSpace(c.FromAddress)
	if name := strings.TrimSpace(c.FromName); name != "" {
		return mime.QEncoding.Encode("utf-8", name) + " <" + from + ">"
	}
	return from
}

// Link makes an absolute URL into this app for a mail body, or "" when no
// base URL is configured (a relative link in a mail is useless).
func (c Config) Link(path string) string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" || path == "" {
		return ""
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

// Notification is the content of one event mail before recipients are
// resolved. Lines is the body; Path (optional) becomes a "바로 열기" link.
type Notification struct {
	Event   string
	Subject string
	Lines   []string
	Path    string
}

// Body renders the plain-text body with the link and a footer explaining
// why the mail arrived.
func (n Notification) Body(c Config) string {
	lines := append([]string{}, n.Lines...)
	if link := c.Link(n.Path); link != "" {
		lines = append(lines, "", "바로 열기: "+link)
	}
	lines = append(lines, "", "—", "이 메일은 Postra 알림 설정에 따라 자동으로 발송되었습니다.")
	return strings.Join(lines, "\n")
}

// Compose builds the RFC 5322 message. Korean subjects are Q-encoded so
// relays and clients that predate UTF-8 headers still show them correctly.
func Compose(c Config, to, subject, body string, now time.Time) []byte {
	var b strings.Builder
	b.WriteString("From: " + c.From() + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("X-Postra-Notification: 1\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeBody(body))
	return []byte(b.String())
}

// normalizeBody uses CRLF line endings. Dot-stuffing is done by net/smtp's
// DATA writer, so it must not be repeated here.
func normalizeBody(body string) string {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	return body
}

// ---------- the notifications this app sends ----------

// SendFailed tells the sender that an outbound mail stopped. Retries happen
// in the background, so the person may not be looking any more.
func SendFailed(subject, why string) Notification {
	return Notification{
		Event:   EventSendFailed,
		Subject: "[Postra] 메일 발송이 멈췄습니다: " + trim(subject, 80),
		Lines: []string{
			fmt.Sprintf("'%s' 메일을 보내지 못했습니다.", trim(subject, 200)),
			"사유: " + why,
			"",
			"발송함에서 상태를 확인하고 필요하면 다시 보내세요.",
		},
		Path: "/app/sent",
	}
}

// Assigned tells somebody a team-inbox message is now their turn.
func Assigned(actor, subject, messageID string) Notification {
	return Notification{
		Event:   EventAssigned,
		Subject: "[Postra] 담당 메일이 배정되었습니다: " + trim(subject, 80),
		Lines: []string{
			fmt.Sprintf("%s 님이 '%s' 메일을 회원님께 배정했습니다.", actor, trim(subject, 200)),
		},
		Path: "/app/mail/" + messageID,
	}
}

// SyncCredentialError tells an account owner that collection has stopped
// until they fix the mailbox password — otherwise the inbox just goes quiet.
func SyncCredentialError(accountName, accountEmail, accountID string) Notification {
	label := accountName
	if accountEmail != "" {
		label = accountName + " (" + accountEmail + ")"
	}
	return Notification{
		Event:   EventSyncCredentialError,
		Subject: "[Postra] 메일 수집이 멈췄습니다: " + trim(label, 80),
		Lines: []string{
			fmt.Sprintf("'%s' 계정의 메일 서버가 로그인을 거부해 자동 수집을 중단했습니다.", trim(label, 200)),
			"계정 설정에서 비밀번호를 다시 등록하면 수집이 재개됩니다.",
		},
		Path: "/app/accounts/" + accountID,
	}
}

// Incident tells administrators that a new critical incident was recorded.
func Incident(component, message string) Notification {
	return Notification{
		Event:   EventIncident,
		Subject: "[Postra] 심각 장애: " + trim(message, 80),
		Lines: []string{
			"구성 요소: " + component,
			"내용: " + trim(message, 500),
			"",
			"운영 콘솔의 시스템 장애 화면에서 상세를 확인하세요.",
		},
		Path: "/app/admin?category=system",
	}
}

// TestMessage proves the relay works from the settings screen.
func TestMessage() Notification {
	return Notification{
		Event:   EventTest,
		Subject: "[Postra] SMTP 발송 테스트",
		Lines:   []string{"Postra 운영 콘솔에서 보낸 테스트 메일입니다.", "이 메일을 받았다면 알림 메일 설정이 정상입니다."},
	}
}

func trim(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
