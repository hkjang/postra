package application

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"postra/internal/platform/notifymail"
)

// Team-inbox deadline notifications (MAIL-STANDARD "만료 임박").
//
// A deadline that passes unnoticed costs somebody a customer or a promise, and
// without a mail the assignee's only option is to keep refreshing the team
// inbox. The leader node walks assigned, unfinished messages whose deadline
// is within slaDueSoonWindow or already behind, and mails each assignee once
// when it becomes imminent and once more when it is missed — never again
// until the deadline itself changes (sla_notified_at < sla_due). Everything
// one person needs to hear is bundled into a single mail per pass.

// slaDueSoonWindow is how far ahead "imminent" reaches.
var slaDueSoonWindow = 24 * time.Hour

// slaNotifyInterval is the leader's polling cadence; the switch and the
// relay's on/off state are re-read every pass so no restart is needed.
var slaNotifyInterval = 5 * time.Minute

// RunSLANotifier mails assignees about imminent and missed deadlines until
// ctx is cancelled. Leader-only and dormant while mail.enabled or the
// mail.notify_sla_due switch is off.
func (a *App) RunSLANotifier(ctx context.Context) {
	a.runLiveWorker(ctx, "sla-notifier", func() time.Duration {
		cfg := a.NotifyMailConfig()
		if !cfg.Enabled || !cfg.Allows(notifymail.EventSLADue) {
			return 0
		}
		return slaNotifyInterval
	}, true, func() { a.NotifySLADeadlines(ctx, time.Now()) })
}

// NotifySLADeadlines runs one pass and returns how many assignees were
// mailed. Exposed for deterministic testing.
func (a *App) NotifySLADeadlines(ctx context.Context, now time.Time) int {
	cfg := a.NotifyMailConfig()
	if !cfg.Enabled || !cfg.Allows(notifymail.EventSLADue) {
		return 0
	}
	ctx = WithActor(ctx, "scheduler")
	rows, err := a.Store.ListSLADueCollab(ctx, now.Add(slaDueSoonWindow).Unix(), 200)
	if err != nil {
		slog.Error("sla notifier: list deadlines failed", "err", err)
		return 0
	}
	type bundle struct {
		items []notifymail.SLAItem
		ids   []string
	}
	byAssignee := map[string]*bundle{}
	var order []string
	for _, mc := range rows {
		due := time.Unix(mc.SLADue, 0)
		// Imminent was already sent (notified while the deadline was still
		// ahead); wait for it to be missed before saying anything else.
		if due.After(now) && mc.SLANotifiedAt != 0 {
			continue
		}
		assignee := strings.TrimSpace(mc.Assignee)
		b := byAssignee[assignee]
		if b == nil {
			b = &bundle{}
			byAssignee[assignee] = b
			order = append(order, assignee)
		}
		subject := ""
		if m, err := a.Store.GetMessage(ctx, mc.UserID, mc.MessageID); err == nil {
			subject = m.Subject
		}
		b.items = append(b.items, notifymail.SLAItem{MessageID: mc.MessageID, Subject: subject, Due: due})
		b.ids = append(b.ids, mc.MessageID)
	}
	sort.Strings(order)
	mailed := 0
	for _, assignee := range order {
		b := byAssignee[assignee]
		// One mail per person per pass; the system is the actor, so nobody
		// is excluded for having set the deadline themselves.
		a.NotifyMail(ctx, notifymail.SLADue(b.items, now), "", []string{assignee})
		mailed++
		// Marked whether or not the mail could be delivered or the assignee
		// resolved to a user: the attempt is in the delivery log, and a
		// deadline must not be re-evaluated every five minutes.
		for _, id := range b.ids {
			if err := a.Store.MarkSLANotified(ctx, id, now.Unix()); err != nil {
				slog.Warn("sla notifier: mark failed", "message", id, "err", err)
			}
		}
	}
	if mailed > 0 {
		slog.Info("sla deadline notifications sent", "assignees", mailed, "messages", len(rows))
	}
	return mailed
}
