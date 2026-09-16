package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/notifymail"
)

// Event notifications through the company SMTP relay (MAIL-STANDARD).
//
// Nothing here blocks a request: NotifyMail resolves recipients and returns,
// the relay conversation happens in a background goroutine, and every attempt
// is recorded so an administrator can see what left the building. The
// recipient directory is the existing user table — an account id or login id
// is turned into an address, nothing more.

// notifyMailPreference maps an event to the personal notification category
// the recipient already controls in their settings, so the receiver decides.
var notifyMailPreference = map[string]string{
	notifymail.EventSendFailed:          "notifications.send",
	notifymail.EventAssigned:            "notifications.action",
	notifymail.EventSyncCredentialError: "notifications.sync",
	notifymail.EventIncident:            "notifications.security",
	notifymail.EventSLADue:              "notifications.action",
}

// NotifyMailConfig reads the relay configuration from the live settings. The
// password is never part of it — only the SecretStore reference.
func (a *App) NotifyMailConfig() notifymail.Config {
	values := map[string]string{}
	for _, d := range notifyMailDefinitions() {
		values[d.Key] = a.Setting(d.Key)
	}
	return notifymail.ConfigFromValues(values)
}

// NotifyMail sends one event mail to each recipient that resolves to an
// active user with an address, in the background. actorID (a user id) is
// dropped from the recipients so nobody is told about their own action;
// pass "" for events the system caused. Recipients may be user ids or login
// ids. Failures never surface to the caller.
func (a *App) NotifyMail(ctx context.Context, n notifymail.Notification, actorID string, recipients []string) {
	cfg := a.NotifyMailConfig()
	if !cfg.Enabled || !cfg.Allows(n.Event) || len(recipients) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := cfg.Validate(); err != nil {
		// Switched on but incomplete: leave a reason where an admin looks,
		// deduplicated by message so a busy hour is one row, not a flood.
		slog.Warn("notification mail skipped: relay is not configured", "event", n.Event, "err", err)
		a.recordIncident(domain.SeverityWarning, "notify-mail", "알림 메일이 켜져 있지만 보낼 수 없음: "+err.Error(), "")
		return
	}
	users := a.resolveNotifyRecipients(ctx, recipients, actorID, notifyMailPreference[n.Event])
	if len(users) == 0 {
		return
	}
	body := n.Body(cfg)
	nowTS := time.Now().Unix()
	for _, u := range users {
		d := domain.MailDelivery{
			ID: persistence.NewID("mdl"), Event: n.Event, Recipient: u.Email, Subject: n.Subject,
			UserID: u.ID, ActorID: actorID, Status: domain.MailDeliveryQueued, CreatedAt: nowTS, UpdatedAt: nowTS,
		}
		if err := a.Store.RecordMailDelivery(ctx, &d); err != nil {
			slog.Warn("notification mail was not recorded", "event", n.Event, "err", err)
		}
		raw := notifymail.Compose(cfg, u.Email, n.Subject, body, time.Now())
		a.workerGroup.Add(1)
		go func(d domain.MailDelivery) {
			defer a.workerGroup.Done()
			// Not a.guard: a panic here must not record an incident that would
			// itself try to send mail again.
			defer func() {
				if r := recover(); r != nil {
					slog.Error("notification mail panic recovered", "event", d.Event, "panic", r)
				}
			}()
			a.deliverNotifyMail(cfg, d, raw)
		}(d)
	}
}

// resolveNotifyRecipients turns ids into unique active users with an address,
// dropping the actor and anyone who switched this category off.
func (a *App) resolveNotifyRecipients(ctx context.Context, recipients []string, actorID, preference string) []domain.User {
	var stored map[string]string
	if preference != "" {
		if s, err := a.Store.GetSettings(ctx); err == nil {
			stored = s
		}
	}
	seen := map[string]bool{}
	var out []domain.User
	for _, id := range recipients {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		u := a.lookupNotifyUser(ctx, id)
		if u == nil || u.ID == actorID || u.Status != domain.UserActive || seen[u.ID] {
			continue
		}
		address := strings.TrimSpace(u.Email)
		if _, err := mail.ParseAddress(address); err != nil || strings.ContainsAny(address, "<>,;\r\n") {
			continue
		}
		if stored != nil && !a.notifyPreferenceAllows(stored, u.ID, preference) {
			continue
		}
		seen[u.ID] = true
		u.Email = address
		out = append(out, *u)
	}
	return out
}

// lookupNotifyUser accepts a user id or a login id (team-inbox assignees are
// login ids), borrowing the user table rather than keeping a directory.
func (a *App) lookupNotifyUser(ctx context.Context, id string) *domain.User {
	if u, err := a.Store.GetUser(ctx, id); err == nil && u != nil {
		return u
	}
	if u, _, err := a.Store.GetUserByLogin(ctx, id); err == nil && u != nil {
		return u
	}
	return nil
}

// notifyPreferenceAllows mirrors PersonalSettings for one notification
// category: an admin lock wins, otherwise the user's own choice, otherwise
// the admin default.
func (a *App) notifyPreferenceAllows(stored map[string]string, userID, key string) bool {
	value := a.Setting(key)
	if v, ok := stored[key]; ok {
		value = v
	}
	if stored["policy.lock."+key] != "true" {
		if v, ok := preferenceValue(stored[preferencePrefix(userID, "")+key]); ok {
			value = v
		}
	}
	return value != "false"
}

// deliverNotifyMail retries once, because a relay that briefly refuses a
// connection is common and losing the notification is worse than a short
// wait. The outcome is recorded either way.
func (a *App) deliverNotifyMail(cfg notifymail.Config, d domain.MailDelivery, raw []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*cfg.Timeout+15*time.Second)
	defer cancel()
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		d.Attempts = attempt
		if err = a.sendNotifyMail(ctx, cfg, d.Recipient, raw); err == nil {
			break
		}
		if attempt == 1 {
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
	a.completeNotifyMail(ctx, d, err)
}

func (a *App) completeNotifyMail(ctx context.Context, d domain.MailDelivery, cause error) {
	status, message := domain.MailDeliverySent, ""
	if cause != nil {
		status, message = domain.MailDeliveryFailed, truncateRunes(cause.Error(), 1000)
		slog.Warn("notification mail failed", "event", d.Event, "recipient", d.Recipient, "err", cause)
	}
	if err := a.Store.CompleteMailDelivery(ctx, d.ID, status, max(d.Attempts, 1), message, time.Now().Unix()); err != nil {
		slog.Warn("notification mail outcome was not recorded", "event", d.Event, "err", err)
	}
}

// sendNotifyMail opens one relay session for one recipient. The password is
// acquired from the SecretStore only here and zeroed by the adapter.
func (a *App) sendNotifyMail(ctx context.Context, cfg notifymail.Config, to string, raw []byte) error {
	opts := domain.SMTPSendOptions{
		Host: cfg.Host, Port: cfg.Port, Security: domain.SecurityNone, AuthMethod: "none",
		InsecureSkipVerify: cfg.SkipVerify, ConnectTimeoutSec: int(cfg.Timeout / time.Second),
	}
	switch cfg.Security {
	case "tls":
		opts.Security = domain.SecurityTLS
	case "starttls":
		opts.Security = domain.SecurityStartTLS
	case "auto":
		opts.OpportunisticTLS = true
	}
	if cfg.Username != "" {
		opts.AuthMethod, opts.Username = "auto", cfg.Username
		if cfg.PasswordRef != "" {
			handle, err := a.Secrets.Acquire(ctx, domain.SecretRef(cfg.PasswordRef), domain.PurposeSMTPAuth)
			if err != nil {
				return fmt.Errorf("알림 SMTP 비밀번호를 읽을 수 없습니다: %w", err)
			}
			opts.Password = handle
		}
	}
	receipt, err := a.SMTP.Send(ctx, opts, domain.Envelope{From: cfg.FromAddress, To: []string{to}}, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if receipt.Uncertain {
		return errors.New("서버 응답이 유실되어 수락 여부를 알 수 없습니다 (전달됐을 수 있음)")
	}
	return nil
}

// ---------- admin surface ----------

// MailDeliveryPage is the admin view of the notification log.
type MailDeliveryPage struct {
	Items   []domain.MailDelivery `json:"items"`
	Summary MailDeliverySummary   `json:"summary"`
}

type MailDeliverySummary struct {
	Total  int64            `json:"total"`
	Status map[string]int64 `json:"status"`
}

func (a *App) AdminListMailDeliveries(ctx context.Context, status string, limit int) (MailDeliveryPage, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return MailDeliveryPage{}, err
	}
	items, err := a.Store.ListMailDeliveries(ctx, domain.MailDeliveryFilter{Status: domain.MailDeliveryStatus(strings.TrimSpace(status)), Limit: limit})
	if err != nil {
		return MailDeliveryPage{}, err
	}
	counts, err := a.Store.MailDeliveryCounts(ctx)
	if err != nil {
		return MailDeliveryPage{}, err
	}
	page := MailDeliveryPage{Items: items, Summary: MailDeliverySummary{Status: map[string]int64{}}}
	for k, v := range counts {
		page.Summary.Status[string(k)] = v
		page.Summary.Total += v
	}
	return page, nil
}

// AdminSendTestMail delivers one test message with the saved settings and
// waits for the outcome, which is what the settings screen's button needs.
// An empty recipient means the calling administrator's own address.
func (a *App) AdminSendTestMail(ctx context.Context, recipient string) (domain.MailDelivery, error) {
	p, err := requireAdmin(ctx)
	if err != nil {
		return domain.MailDelivery{}, err
	}
	cfg := a.NotifyMailConfig()
	if !cfg.Enabled {
		return domain.MailDelivery{}, userErrf("%v. %s 을 켠 뒤 저장하고 다시 시험하세요", notifymail.ErrDisabled, notifymail.KeyEnabled)
	}
	if err := cfg.Validate(); err != nil {
		return domain.MailDelivery{}, userErrf("%v", err)
	}
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		if u, err := a.Store.GetUser(ctx, p.UserID); err == nil && u != nil {
			recipient = strings.TrimSpace(u.Email)
		}
		if recipient == "" {
			return domain.MailDelivery{}, userErrf("관리자 계정에 메일 주소가 없습니다. 받는 주소를 지정하세요")
		}
	}
	if _, err := mail.ParseAddress(recipient); err != nil || strings.ContainsAny(recipient, "<>,;\r\n") {
		return domain.MailDelivery{}, userErrf("받는 주소가 올바르지 않습니다")
	}
	n := notifymail.TestMessage()
	nowTS := time.Now().Unix()
	d := domain.MailDelivery{
		ID: persistence.NewID("mdl"), Event: n.Event, Recipient: recipient, Subject: n.Subject,
		ActorID: p.UserID, Status: domain.MailDeliveryQueued, Attempts: 1, CreatedAt: nowTS, UpdatedAt: nowTS,
	}
	if err := a.Store.RecordMailDelivery(ctx, &d); err != nil {
		return domain.MailDelivery{}, err
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Timeout+5*time.Second)
	defer cancel()
	sendErr := a.sendNotifyMail(sendCtx, cfg, recipient, notifymail.Compose(cfg, recipient, n.Subject, n.Body(cfg), time.Now()))
	a.completeNotifyMail(sendCtx, d, sendErr)
	d.Status, d.UpdatedAt = domain.MailDeliverySent, time.Now().Unix()
	result := "ok"
	if sendErr != nil {
		d.Status, d.Error = domain.MailDeliveryFailed, truncateRunes(sendErr.Error(), 1000)
		result = "failed"
	}
	a.audit(ctx, "mail_test_send", "notify-mail", result, "recipient="+recipient)
	return d, nil
}
