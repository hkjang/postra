package application

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"postra/internal/domain"
	"postra/internal/platform/mailhtml"
)

func TestRenderedDraftUsesOneCanonicalPayload(t *testing.T) {
	app, _, smtp, ai := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	ai.genErr = errors.New("offline provider must not be called")
	sig, err := app.SaveMailSignature(ctx, SaveMailSignatureInput{Name: "회사", BodyHTML: `<p>홍길동 <strong>Postra</strong></p>`})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, To: []string{"a@corp.local"}, Subject: "보고", Body: "# 진행 보고\n\n- 완료\n- 검토", Format: "auto", Template: "report", SignatureID: sig.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(draft.Version.BodyHTML, "<h1") || !strings.Contains(draft.Version.BodyHTML, `data-postra-template="report"`) || draft.Version.BodyText != mailhtml.PlainText(draft.Version.BodyHTML) || strings.Count(draft.Version.BodyText, "홍길동") != 1 || draft.Version.Author != "user" {
		t.Fatalf("rendered draft: %+v", draft.Version)
	}
	_, token, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	draft, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, Format: "auto", Template: "formal", SignatureID: sig.ID})
	if err != nil || strings.Count(draft.Version.BodyText, "홍길동") != 1 {
		t.Fatalf("theme update duplicated signature: %+v %v", draft, err)
	}
	if _, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token}); err == nil {
		t.Fatal("theme edit kept old approval")
	}
	preview, token, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token, IdempotencyKey: "rendered-golden"})
	if err != nil || sent.Status != domain.OutboundSent {
		t.Fatalf("send: %+v %v", sent, err)
	}
	again, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token, IdempotencyKey: "rendered-golden"})
	if err != nil || again.ID != sent.ID || len(smtp.sent) != 1 {
		t.Fatalf("idempotency regressed: %+v %v", again, err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(smtp.sent[0].raw))
	if err != nil {
		t.Fatal(err)
	}
	kind, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/alternative" {
		t.Fatalf("wrong MIME type %s %v", kind, err)
	}
	parts := multipart.NewReader(message.Body, params["boundary"])
	for _, want := range []struct{ contentType, body string }{{"text/plain", preview.Body}, {"text/html", preview.BodyHTML}} {
		part, err := parts.NextRawPart()
		if err != nil {
			t.Fatal(err)
		}
		kind, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		body, err := io.ReadAll(quotedprintable.NewReader(part))
		if err != nil || kind != want.contentType || strings.ReplaceAll(string(body), "\r\n", "\n") != want.body {
			t.Fatalf("MIME/preview mismatch: %s %v", kind, err)
		}
	}
	if _, err := parts.NextRawPart(); err != io.EOF {
		t.Fatalf("extra MIME content: %v", err)
	}
}

func TestSignatureOwnershipPersistenceAndAccountRestriction(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	sig, err := app.SaveMailSignature(ctx, SaveMailSignatureInput{Name: "내 서명", AccountID: acc.ID, BodyHTML: `<p onclick="bad()">비공개 서명</p><img src="https://tracker.test/a">`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sig.BodyHTML, "onclick") || strings.Contains(sig.BodyHTML, "<img") {
		t.Fatal("unsafe signature stored")
	}
	loaded, err := app.GetMailSignature(ctx, sig.ID)
	if err != nil || *loaded != *sig {
		t.Fatalf("persistent read differs: %+v %v", loaded, err)
	}
	other := WithPrincipal(ctx, domain.Principal{UserID: "different-user", Role: domain.RoleAdmin})
	if list, err := app.ListMailSignatures(other); err != nil || len(list) != 0 {
		t.Fatalf("admin read another person's signatures: %+v %v", list, err)
	}
	if _, err := app.GetMailSignature(other, sig.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user get: %v", err)
	}
	if _, err := app.SaveMailSignature(other, SaveMailSignatureInput{ID: sig.ID, Name: "overwrite", Body: "malicious"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user save: %v", err)
	}
	if err := app.DeleteMailSignature(other, sig.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-user delete: %v", err)
	}
	if _, err := app.RenderMail(ctx, RenderMailInput{Body: "body", SignatureID: sig.ID}); err == nil {
		t.Fatal("account-restricted signature used without account")
	}
	if _, err := app.RenderMail(ctx, RenderMailInput{AccountID: acc.ID, Body: "body", SignatureID: sig.ID}); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteMailSignature(ctx, sig.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetMailSignature(ctx, sig.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted signature still readable: %v", err)
	}
	if list, err := app.ListMailSignatures(ctx); err != nil || len(list) != 0 {
		t.Fatalf("deleted signature listed: %+v %v", list, err)
	}
}

func TestRendererPreferencesAndForcedPolicies(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	if _, err := app.SavePersonalSettings(ctx, acc.ID, SettingsPatch{Values: map[string]string{"compose.template": "notice", "compose.format": "markdown"}}); err != nil {
		t.Fatal(err)
	}
	out, err := app.RenderMail(ctx, RenderMailInput{AccountID: acc.ID, Body: "# 계정 기본값"})
	if err != nil || out.Template != "notice" || out.Format != "markdown" {
		t.Fatalf("account preference ignored: %+v %v", out, err)
	}
	app.applyRuntimeSettings(map[string]string{"mail.html_enabled": "false"})
	out, err = app.RenderMail(ctx, RenderMailInput{AccountID: acc.ID, Body: "# 정책", Template: "newsletter", Format: "markdown"})
	if err != nil || out.BodyHTML != "" || out.Template != "plain" {
		t.Fatalf("HTML policy bypassed: %+v %v", out, err)
	}
	legacy, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, BodyHTML: "<p>Legacy</p>"})
	if err != nil || legacy.Version.BodyHTML != "" || legacy.Version.BodyText != "Legacy" {
		t.Fatalf("legacy HTML bypassed policy: %+v %v", legacy, err)
	}
}

func TestSmartFormattingRequiresExplicitAIAndScope(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	ai.response = `{"subject":"not used","body":"## 다듬은 본문\n\n- 완료"}`
	keyCtx := WithPrincipal(ctx, domain.Principal{UserID: DefaultUserID, AuthMethod: "mcp_key", MCPScopes: []string{"mail.draft"}})
	if _, err := app.RenderMail(keyCtx, RenderMailInput{Body: "원문", SmartFormat: true}); err == nil {
		t.Fatal("smart format bypassed mail.ai scope")
	}
	if _, err := app.RenderMail(keyCtx, RenderMailInput{Body: "원문", Format: "auto"}); err != nil {
		t.Fatal(err)
	}
	out, err := app.RenderMail(ctx, RenderMailInput{Body: "원문", SmartFormat: true})
	if err != nil || !strings.Contains(out.BodyHTML, "<h2") || !strings.Contains(out.BodyText, "다듬은 본문") {
		t.Fatalf("structured smart formatting failed: %+v %v", out, err)
	}
}

func TestForcedPreferencesAndSelectedTextDoNotMixSignatures(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	admin := WithPrincipal(ctx, domain.Principal{UserID: DefaultUserID, Role: domain.RoleAdmin})
	sig, err := app.SaveMailSignature(ctx, SaveMailSignatureInput{Name: "구조화 서명", DisplayName: "홍길동", Company: "Postra", Email: "hong@corp.local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.SavePersonalSettings(ctx, "", SettingsPatch{Values: map[string]string{"compose.signature_id": sig.ID}}); err != nil {
		t.Fatal(err)
	}
	_, err = app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{"compose.template": "notice", "compose.use_signature": "true"}, Locks: map[string]bool{"compose.template": true, "compose.use_signature": true}})
	if err != nil {
		t.Fatal(err)
	}
	no := false
	out, err := app.RenderMail(ctx, RenderMailInput{Body: "본문", Template: "newsletter", UseSignature: &no})
	if err != nil || out.Template != "notice" || !strings.Contains(out.BodyText, "홍길동") {
		t.Fatalf("explicit options bypassed locked defaults: %+v %v", out, err)
	}
	ai.response = `{"body":"선택한 내용만 수정"}`
	selected, err := app.RewriteMailText(ctx, RewriteMailTextInput{Text: "선택한 원문"})
	if err != nil || selected != "선택한 내용만 수정" || strings.Contains(selected, "홍길동") {
		t.Fatalf("forced signature polluted selected range: %q %v", selected, err)
	}
	if err := app.DeleteMailSignature(ctx, sig.ID); err != nil {
		t.Fatal(err)
	}
	out, err = app.RenderMail(ctx, RenderMailInput{Body: "본문"})
	if err != nil || out.BodyText != "본문" || len(out.Warnings) == 0 {
		t.Fatalf("deleted default signature blocked all new mail: %+v %v", out, err)
	}
}

func TestSmartSignatureFirstReplyAndKnownSubsequentReply(t *testing.T) {
	app, pop, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	pop.messages["uid1"] = testMail("signature-thread", "대화", "원문")
	syncAndWait(t, app, acc.ID)
	messages, err := app.Search(ctx, domain.SearchQuery{AccountID: acc.ID, Limit: 10})
	if err != nil || len(messages.Messages) == 0 {
		t.Fatalf("messages: %+v %v", messages, err)
	}
	sig, err := app.SaveMailSignature(ctx, SaveMailSignatureInput{Name: "전체 서명", DisplayName: "홍길동", Company: "Postra", Email: "hong@corp.local", Body: "홍길동\nPostra\nhong@corp.local\n추가 안내"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.SavePersonalSettings(ctx, acc.ID, SettingsPatch{Values: map[string]string{"compose.signature_id": sig.ID, "compose.signature_policy": "smart"}}); err != nil {
		t.Fatal(err)
	}
	input := CreateDraftInput{AccountID: acc.ID, Kind: "reply", ReplyToMessageID: messages.Messages[0].ID, Body: "회신 본문", Format: "auto"}
	first, err := app.CreateDraft(ctx, input)
	if err != nil || !strings.Contains(first.Version.BodyText, "홍길동") || strings.Contains(first.Version.BodyText, "추가 안내") {
		t.Fatalf("first reply compact signature: %+v %v", first, err)
	}
	_, token, err := app.RequestSendApproval(ctx, first.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Send(ctx, SendInput{DraftID: first.Draft.ID, ApprovalToken: token.Token}); err != nil {
		t.Fatal(err)
	}
	next, err := app.CreateDraft(ctx, input)
	if err != nil || strings.Contains(next.Version.BodyText, "홍길동") {
		t.Fatalf("subsequent reply repeated signature: %+v %v", next, err)
	}
}

func TestSendPoliciesAreRecheckedAfterApprovalAndBeforeRetry(t *testing.T) {
	for _, policy := range []struct{ key, value string }{{"mail.smtp_enabled", "false"}, {"mail.tls_required", "true"}, {"mail.smtp_auth_required", "true"}, {"mail.html_enabled", "false"}, {"send.max_recipients", "1"}} {
		t.Run(policy.key, func(t *testing.T) {
			app, _, smtp, _ := newTestApp(t)
			ctx := WithActor(context.Background(), "test")
			acc := mustAccount(t, app)
			draft, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, To: []string{"a@corp.local", "b@corp.local"}, Subject: "test", Body: "Body", Format: "auto"})
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
			if err != nil {
				t.Fatal(err)
			}
			app.applyRuntimeSettings(map[string]string{policy.key: policy.value})
			if _, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token}); err == nil || len(smtp.sent) != 0 {
				t.Fatalf("new policy bypassed: %v", err)
			}
			if _, err := app.deliver(ctx, &domain.OutboundMessage{DraftID: draft.Draft.ID}, acc, &draft.Version); err == nil || len(smtp.sent) != 0 {
				t.Fatalf("retry bypassed new policy: %v", err)
			}
		})
	}
}

func TestOutboundImagesAllowProxyAndChangedPolicy(t *testing.T) {
	requests := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) }))
	defer origin.Close()
	app, _, smtp, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	app.applyRuntimeSettings(map[string]string{"mail.outbound_images": "allow"})
	draft, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, To: []string{"a@corp.local"}, Subject: "image", BodyHTML: `<p>본문<img src="` + origin.URL + `/logo.png" alt="회사 로고"></p>`, Format: "html"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(draft.Version.BodyHTML, origin.URL) || !strings.Contains(draft.Version.BodyHTML, "data-postra-external-image") {
		t.Fatalf("allow image dropped: %s", draft.Version.BodyHTML)
	}
	_, token, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	app.applyRuntimeSettings(map[string]string{"mail.outbound_images": "block"})
	if _, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token}); err == nil || len(smtp.sent) != 0 {
		t.Fatal("policy tightening silently changed approved images")
	}
	proxy := "https://images.corp.local/proxy"
	app.applyRuntimeSettings(map[string]string{"mail.outbound_images": "proxy", "mail.image_proxy_url": proxy})
	draft, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, Format: "html"})
	if err != nil {
		t.Fatal(err)
	}
	sources := mailhtml.RemoteImageSources(draft.Version.BodyHTML)
	if len(sources) != 1 {
		t.Fatalf("proxy image missing: %s", draft.Version.BodyHTML)
	}
	u, _ := url.Parse(sources[0])
	if u.Query().Get("url") != origin.URL+"/logo.png" {
		t.Fatalf("target not encoded: %s", sources[0])
	}
	_, token, err = app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	out, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token})
	if err != nil || out.Status != domain.OutboundSent {
		t.Fatalf("proxy send: %+v %v", out, err)
	}
	if requests != 0 {
		t.Fatal("Postra fetched an outbound image (SSRF/tracking)")
	}
}

func TestCanonicalHTMLMIMEGolden(t *testing.T) {
	account := &domain.MailAccount{Name: "Sender", Email: "sender@corp.local"}
	version := &domain.DraftVersion{Subject: "Golden mail", To: []domain.Address{{Email: "recipient@corp.local"}}, BodyHTML: "<p>Hello</p>"}
	raw, err := buildMIME(account, version, "<golden@postra.local>", "", "")
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(message.Header.Get("Content-Type"))
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, params["boundary"], "BOUNDARY")
	normalized = regexp.MustCompile(`(?m)^Date: .*$`).ReplaceAllString(normalized, "Date: FIXED")
	golden, err := os.ReadFile("testdata/rendered-mail.golden.eml")
	if err != nil {
		t.Fatal(err)
	}
	if normalized != string(golden) {
		t.Fatalf("MIME golden mismatch:\n%s", normalized)
	}
}
