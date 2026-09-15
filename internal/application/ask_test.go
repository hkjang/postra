package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
)

func TestAskDateIntentRouter(t *testing.T) {
	now := time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC)
	in, err := askPlan(AskInput{Question: "지난주 미완료 업무", TimeZone: "Asia/Seoul"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if in.Mode != "work" || !in.IncompleteOnly || time.Unix(in.Since, 0).UTC().Format(time.RFC3339) != "2026-09-06T15:00:00Z" || time.Unix(in.Until, 0).UTC().Format(time.RFC3339) != "2026-09-13T15:00:00Z" {
		t.Fatalf("bad router: %+v", in)
	}
	in, err = askPlan(AskInput{Question: "어제 메일", Since: 10, Until: 20}, now)
	if err != nil || in.Since != 10 || in.Until != 20 {
		t.Fatalf("explicit date overridden: %+v %v", in, err)
	}
	for _, invalid := range []AskInput{{Question: "x", Mode: "anything"}, {Question: "x", TimeZone: "invalid/zone"}, {Question: "x", Since: 30, Until: 10}, {Question: strings.Repeat("가", 4001)}} {
		if _, err := askPlan(invalid, now); err == nil {
			t.Fatalf("invalid plan accepted: %+v", invalid)
		}
	}
}

func TestAskEvidenceScopeWorkAndNoKeywordFallback(t *testing.T) {
	a, _, _, ai := newTestApp(t)
	ctx := settingsAdmin()
	account := mustAccount(t, a)
	for _, id := range []string{"ask-own", "ask-complete"} {
		m := &domain.Message{ID: id, UserID: DefaultUserID, AccountID: account.ID, UIDL: id, Subject: "견적서 검토", From: domain.Address{Email: "sender@corp.local"}, Date: 100, RawHash: id, RawURI: "missing:" + id}
		if err := a.Store.InsertMessage(ctx, m, &domain.MessageBody{MessageID: id, TextBody: "요청한 견적서를 검토하고 회신해주세요."}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.SetMessageWorkStatus(ctx, "ask-own", domain.CollabInProgress); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetMessageWorkStatus(ctx, "ask-complete", domain.CollabDone); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.CreateActionCard(ctx, &domain.ActionCard{ID: "ask-action", UserID: DefaultUserID, MessageID: "ask-own", Title: "비용 확인", Status: domain.ActionCardPending, Due: "2026-09-20"}); err != nil {
		t.Fatal(err)
	}
	ai.response = `{"answer":"비용 확인이 진행 중입니다.","evidence_message_ids":["ask-own","other-user-message"],"confidence":0.8}`
	result, err := a.Ask(ctx, AskInput{Question: "미완료 업무", Mode: "work", IncompleteOnly: true, SearchText: "견적서"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Retrieval.Sources) != 1 || result.Retrieval.Sources[0].MessageID != "ask-own" || len(result.Retrieval.Sources[0].ActionIDs) != 1 {
		t.Fatalf("wrong sources: %+v", result.Retrieval)
	}
	if strings.Contains(result.ResultJSON, "other-user-message") || !strings.Contains(ai.lastRequest.Untrusted, "비용 확인") || strings.Contains(ai.lastRequest.Untrusted, "ask-complete") {
		t.Fatal("evidence scope or citation violated")
	}
	if _, err := a.Ask(ctx, AskInput{Question: "메일", SearchText: "does-not-exist", Mode: "keyword"}); err == nil {
		t.Fatal("explicit keyword constraint silently broadened")
	}
	if _, err := a.Ask(ctx, AskInput{Question: "메일", Since: 200, Until: 300, Mode: "keyword"}); err == nil {
		t.Fatal("date constraint silently broadened")
	}
	if err := a.Store.EnsureUser(ctx, "ask-other", "ask-other"); err != nil {
		t.Fatal(err)
	}
	other := WithPrincipal(context.Background(), domain.Principal{UserID: "ask-other", Role: domain.RoleAdmin})
	if _, err := a.Ask(other, AskInput{Question: "업무", Mode: "work"}); err == nil {
		t.Fatal("other administrator saw private work")
	}
	if _, err := a.Ask(other, AskInput{Question: "메일", AccountID: account.ID}); err == nil {
		t.Fatal("other administrator accessed private account")
	}
	ai.response = `{"answer":"내용 부족","evidence_message_ids":"ask-own"}`
	result, err = a.Ask(ctx, AskInput{Question: "unknown-fallback-question", Mode: "keyword"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Retrieval.Warnings) == 0 {
		t.Fatal("recent fallback not disclosed")
	}
	var answer struct {
		IDs []string `json:"evidence_message_ids"`
	}
	if err := json.Unmarshal([]byte(result.ResultJSON), &answer); err != nil || len(answer.IDs) != 1 {
		t.Fatalf("valid string citation wasn't normalized: %s", result.ResultJSON)
	}
}

func TestDeploymentEncryptionCannotBeOverriddenByLegacySetting(t *testing.T) {
	a, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	if err := a.Store.UpsertSettings(ctx, map[string]string{SettingEncryptAtRest: "false"}); err != nil {
		t.Fatal(err)
	}
	next, err := New(a.initialConfig, a.Store, a.Objects, a.Secrets, a.POP3, a.SMTP, a.aiRaw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Shutdown)
	if !next.EffectiveConfig().EncryptAtRest || !next.SettingBool(SettingEncryptAtRest) {
		t.Fatal("legacy setting mismatched actual encrypted storage")
	}
	view, err := next.AdminSettingsCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if field := settingField(t, view, SettingEncryptAtRest); field.Value != "true" || !field.Locked || field.Source == "admin" {
		t.Fatalf("misleading encryption status: %+v", field)
	}
}
