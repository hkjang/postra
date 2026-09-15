package application

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"postra/internal/domain"
)

func TestLiveWorkerScheduleEnableDisableAndIntervalChanges(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	state := workerSchedule{}
	if state.due(now, 0, false) {
		t.Fatal("startup-disabled worker executed")
	}
	if !state.due(now.Add(time.Second), 10*time.Minute, false) {
		t.Fatal("enabling a startup-disabled worker did not activate it")
	}
	if state.due(now.Add(time.Minute), 10*time.Minute, false) {
		t.Fatal("worker ignored its interval")
	}
	if !state.due(now.Add(2*time.Minute), time.Minute, false) {
		t.Fatal("shortened interval was not applied")
	}
	if state.due(now.Add(3*time.Minute), time.Hour, false) {
		t.Fatal("lengthened interval was not applied")
	}
	if state.due(now.Add(4*time.Minute), 0, false) || !state.due(now.Add(5*time.Minute), time.Hour, false) {
		t.Fatal("disable/re-enable transition lost")
	}
	delayed := workerSchedule{}
	if delayed.due(now, time.Minute, false) || !delayed.due(now.Add(time.Minute), time.Minute, false) {
		t.Fatal("startup cadence changed unexpectedly")
	}
	if minutesDuration(1<<40) <= 0 {
		t.Fatal("large imported cadence overflowed duration")
	}
}

func TestLiveWorkerSettingsAndLegacyTriageMode(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	admin := settingsAdmin()
	if app.triageWorkerInterval() != 0 || app.digestWorkerInterval() != 0 {
		t.Fatal("opt-in workers unexpectedly enabled")
	}
	app.Cfg.Sync.AutoTriage = true
	if app.triageMode() != "all" {
		t.Fatal("legacy auto-triage opt-in lost")
	}
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{
		"sync.triage_mode": "important_only", "sync.auto_triage_minutes": "2",
		"sync.daily_digest_enabled": "true", "sync.auto_embed_minutes": "9",
	}}); err != nil {
		t.Fatal(err)
	}
	if app.triageMode() != "important_only" || app.triageWorkerInterval() != 2*time.Minute || app.digestWorkerInterval() == 0 || app.EffectiveConfig().Sync.AutoEmbedMinutes != 9 {
		t.Fatal("persisted worker settings did not hot apply")
	}
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{
		"sync.triage_mode": "off", "sync.daily_digest_enabled": "false", "sync.auto_embed_minutes": "0",
	}}); err != nil {
		t.Fatal(err)
	}
	if app.triageWorkerInterval() != 0 || app.digestWorkerInterval() != 0 || app.EffectiveConfig().Sync.AutoEmbedMinutes != 0 {
		t.Fatal("explicit disable failed, possibly overridden by legacy flag")
	}
}

func TestDynamicSyncLimiterPreservesRunningJobs(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	setLimit := func(value string) {
		t.Helper()
		if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: map[string]string{"sync.max_concurrent_syncs": value}}); err != nil {
			t.Fatal(err)
		}
	}
	setLimit("1")
	if ok, _ := app.tryAcquireSyncSlot(); !ok {
		t.Fatal("first slot rejected")
	}
	if ok, _ := app.tryAcquireSyncSlot(); ok {
		t.Fatal("exceeded configured limit")
	}
	setLimit("3")
	for i := 0; i < 2; i++ {
		if ok, _ := app.tryAcquireSyncSlot(); !ok {
			t.Fatal("live increase not applied")
		}
	}
	setLimit("1")
	for remaining := 3; remaining > 0; remaining-- {
		ok, changed := app.tryAcquireSyncSlot()
		if ok || app.syncSlotsInUse != remaining {
			t.Fatal("decrease cancelled a running slot or admitted extra work")
		}
		app.releaseSyncSlot()
		select {
		case <-changed:
		default:
			t.Fatal("release did not wake queued jobs")
		}
	}
	if ok, _ := app.tryAcquireSyncSlot(); !ok {
		t.Fatal("slot never became available after draining")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.acquireSyncSlot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queue wait: %v", err)
	}
	app.releaseSyncSlot()
}

func TestAccountSyncIntervalPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		org, floor int
		prefs      map[string]string
		want       time.Duration
	}{
		{"organization disabled", 0, 1, map[string]string{"account.sync_minutes": "1"}, 0},
		{"account disabled", 5, 1, map[string]string{"account.auto_sync": "false"}, 0},
		{"inherit", 5, 1, nil, 5 * time.Minute},
		{"floor", 5, 3, map[string]string{"account.sync_minutes": "1"}, 3 * time.Minute},
		{"account cadence", 5, 1, map[string]string{"account.sync_minutes": "12"}, 12 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountSyncInterval(tc.org, tc.floor, tc.prefs); got != tc.want {
				t.Fatalf("interval=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestSchedulerHonorsPersistedAccountPreferences(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	admin := settingsAdmin()
	first := mustAccount(t, app)
	second := *first
	second.ID = "acc_scheduler_second"
	if err := app.Store.CreateAccount(admin, &second); err != nil {
		t.Fatal(err)
	}
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{"sync.auto_sync_minutes": "5", "sync.min_sync_minutes": "3"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SavePersonalSettings(admin, first.ID, SettingsPatch{Values: map[string]string{"account.auto_sync": "false"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SavePersonalSettings(admin, second.ID, SettingsPatch{Values: map[string]string{"account.sync_minutes": "1"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	last := map[string]time.Time{first.ID: now, second.ID: now, "deleted-account": now}
	app.syncActiveAccounts(admin, now.Add(2*time.Minute), last)
	if _, exists := last["deleted-account"]; exists {
		t.Fatal("deleted accounts leak scheduler state")
	}
	if !last[second.ID].Equal(now) {
		t.Fatal("account preference bypassed organization minimum")
	}
	app.syncActiveAccounts(admin, now.Add(3*time.Minute), last)
	app.workerGroup.Wait()
	jobs, err := app.ListJobs(admin, 100)
	if err != nil || len(jobs) != 1 || jobs[0].AccountID != second.ID {
		t.Fatalf("wrong accounts scheduled: %+v, %v", jobs, err)
	}
	if !last[first.ID].Equal(now) || !last[second.ID].Equal(now.Add(3*time.Minute)) {
		t.Fatal("account opt-out or cadence not respected")
	}
}

func TestTriageModesFilterBeforeAIAndPagePastNonmatches(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ctx := settingsAdmin()
	acc := mustAccount(t, app)
	for i := 0; i < 105; i++ {
		recentMessage(t, app, ctx, acc.ID, fmt.Sprintf("msg_noise_%03d", i), "일반 공지", "일반 본문")
	}
	target := recentMessage(t, app, ctx, acc.ID, "msg_triage_target", "처리 요청", "rule-specific-content")
	target.IsImportant, target.Date = true, 1 // older sort order, same recent ingestion
	if err := app.Store.UpdateMessage(ctx, target); err != nil {
		t.Fatal(err)
	}
	rule, err := app.CreateRule(ctx, domain.MailRule{
		Name: "분류 후보", Enabled: true, Match: "all",
		Conditions: []domain.RuleCondition{{Field: domain.RuleFieldBody, Operator: domain.RuleOpContains, Value: "rule-specific-content"}},
		Actions:    []domain.RuleAction{{Type: domain.RuleActionAddLabel, Value: "matched"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"important_only", "rules"} {
		ids, err := app.triageCandidates(ctx, time.Now().Add(-time.Hour).Unix(), mode)
		if err != nil || len(ids) != 1 || ids[0] != target.ID {
			t.Fatalf("mode %s starved eligible mail: %v %v", mode, ids, err)
		}
	}
	if n := app.triageOnceMode(ctx, "off"); n != 0 {
		t.Fatal("disabled mode invoked AI")
	}
	ai.response = `{"priority":"high","reply_required":false}`
	if n := app.triageOnceMode(ctx, "rules"); n != 1 {
		t.Fatalf("rule-filtered classification count %d, want 1", n)
	}
	if n := app.triageOnceMode(ctx, "important_only"); n != 0 {
		t.Fatal("already-triaged important mail was sent to AI again")
	}
	noise, _ := app.Store.GetMessage(ctx, DefaultUserID, "msg_noise_000")
	if len(noise.Labels) != 0 {
		t.Fatal("nonmatching mail classified or rule action unexpectedly applied")
	}
	rule.Enabled = false
	if _, err := app.UpdateRule(ctx, *rule); err != nil {
		t.Fatal(err)
	}
	if triageMatches("rules", &domain.Message{}, &domain.MessageBody{TextBody: "rule-specific-content"}, []domain.MailRule{*rule}) {
		t.Fatal("disabled rule still matched")
	}
}
