package application

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"postra/internal/domain"
)

func TestHTMLDraftApprovalAndMultipartDelivery(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	draft, err := app.CreateDraft(ctx, CreateDraftInput{
		AccountID: acc.ID, To: []string{"alice@corp.local"}, Subject: "주간 진행 현황 ✨",
		Body: "old plain text", BodyHTML: `<h2 style="color:#2563eb">안녕하세요 ✨</h2><p>이번 주 <strong>업무 완료</strong>했습니다.</p><script>alert(1)</script>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(draft.Version.BodyHTML, "script") || strings.Contains(draft.Version.BodyText, "old plain text") || !strings.Contains(draft.Version.BodyText, "업무 완료") {
		t.Fatalf("HTML must be sanitized and drive both alternatives: %+v", draft.Version)
	}
	loaded, err := app.GetDraft(ctx, draft.Draft.ID)
	if err != nil || loaded.Version.BodyHTML != draft.Version.BodyHTML {
		t.Fatalf("HTML draft did not survive persistence: %+v, %v", loaded, err)
	}
	preview, approval, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil || preview.BodyHTML != draft.Version.BodyHTML || preview.Body != draft.Version.BodyText {
		t.Fatalf("preview must match both alternatives: %+v, %v", preview, err)
	}
	// Only presentation changes; it is still a different approved payload.
	changedHTML := strings.Replace(draft.Version.BodyHTML, "#2563eb", "#15803d", 1)
	changedVersion := draft.Version
	changedVersion.BodyHTML = changedHTML
	if sendPayloadHash(acc, &changedVersion) == sendPayloadHash(acc, &draft.Version) {
		t.Fatal("approval hash must include HTML even if plain text and version are identical")
	}
	draft, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, BodyHTML: &changedHTML, Bcc: []string{"hidden@corp.local"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: approval.Token}); err == nil {
		t.Fatal("HTML presentation change must invalidate prior approval")
	}
	_, approval, err = app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	out, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: approval.Token})
	if err != nil || out.Status != domain.OutboundSent || len(smtp.sent) != 1 {
		t.Fatalf("HTML send failed: %+v, %v", out, err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(smtp.sent[0].raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("Bcc") != "" || len(smtp.sent[0].env.To) != 2 {
		t.Fatal("Bcc must remain envelope-only")
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != draft.Version.Subject {
		t.Fatalf("Unicode subject changed: %q, %v", subject, err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" || params["boundary"] == "" {
		t.Fatalf("HTML message needs multipart/alternative: %s, %v", msg.Header.Get("Content-Type"), err)
	}
	reader := multipart.NewReader(msg.Body, params["boundary"])
	for _, want := range []struct{ mediaType, value string }{{"text/plain", draft.Version.BodyText}, {"text/html", draft.Version.BodyHTML}} {
		part, err := reader.NextRawPart()
		if err != nil {
			t.Fatal(err)
		}
		partType, partParams, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || partType != want.mediaType || partParams["charset"] != "utf-8" || part.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
			t.Fatalf("invalid MIME alternative: %v, %v", part.Header, err)
		}
		body, err := io.ReadAll(quotedprintable.NewReader(part))
		if err != nil || strings.ReplaceAll(string(body), "\r\n", "\n") != want.value {
			t.Fatalf("%s body changed: %q != %q (%v)", want.mediaType, body, want.value, err)
		}
	}
	if _, err := reader.NextRawPart(); err != io.EOF {
		t.Fatalf("multipart message was not properly terminated: %v", err)
	}
}

func TestHTMLDraftPlainEditAndAIRewriteClearOldHTML(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	draft, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, To: []string{"alice@corp.local"}, Subject: "test", BodyHTML: "<p>Old <b>HTML</b></p>"})
	if err != nil {
		t.Fatal(err)
	}
	subject := "Changed subject"
	draft, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, Subject: &subject})
	if err != nil || draft.Version.BodyHTML == "" {
		t.Fatalf("unrelated edits must preserve formatting: %+v, %v", draft, err)
	}
	ai.response = `{"subject":"Rewritten","body":"Fresh AI text"}`
	draft, err = app.RewriteDraft(ctx, draft.Draft.ID, "concise")
	if err != nil || draft.Version.BodyHTML != "" || draft.Version.BodyText != "Fresh AI text" {
		t.Fatalf("AI rewrite retained stale HTML: %+v, %v", draft, err)
	}
	html := "<p>Restored <b>HTML</b></p>"
	draft, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, BodyHTML: &html})
	if err != nil {
		t.Fatal(err)
	}
	plain := "Fresh plain text"
	draft, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, Body: &plain})
	if err != nil || draft.Version.BodyHTML != "" || draft.Version.BodyText != plain {
		t.Fatalf("plain edit retained stale HTML: %+v, %v", draft, err)
	}
	raw, err := buildMIME(acc, &draft.Version, "<plain@corp.local>", "", "")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil || strings.Contains(msg.Header.Get("Content-Type"), "multipart") {
		t.Fatalf("plain-text compatibility regressed: %v", err)
	}
}

func TestHTMLDLPChecksAlternativeAndExternalBcc(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	app.Cfg.Send.DLPPolicy = "block"
	app.Cfg.Send.DLPKeywords = []string{"confidential"}
	draft, err := app.CreateDraft(ctx, CreateDraftInput{
		AccountID: acc.ID, To: []string{"alice@corp.local"}, Subject: "test",
		Body: "innocent text", BodyHTML: `<p>confi<strong>dential</strong></p>`,
	})
	if err != nil || draft.Version.BodyText != "confidential" {
		t.Fatalf("HTML-only payload must derive scanned text: %+v, %v", draft, err)
	}
	_, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, Bcc: []string{"hidden@external.test"}})
	if err != nil {
		t.Fatal(err)
	}
	preview, token, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil || !preview.DLPBlocked || len(preview.DLPFindings) == 0 {
		t.Fatalf("HTML DLP missed external Bcc: %+v, %v", preview, err)
	}
	if _, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token}); err == nil || !strings.Contains(err.Error(), "DLP") || len(smtp.sent) != 0 {
		t.Fatalf("HTML DLP must block before SMTP: %v", err)
	}
	// Existing/imported versions can contain mismatched alternatives. Check
	// both at the actual delivery gate, not just the normal compose route.
	v := domain.DraftVersion{To: []domain.Address{{Email: "a@external.test"}}, BodyText: "safe", BodyHTML: "<p>confi<b>dential</b></p>"}
	if err := app.enforceDLP(acc, &v); err == nil {
		t.Fatal("mismatched HTML alternative bypassed DLP")
	}
	v.BodyHTML = `<p><a href="https://corp.local" title="confidential">safe</a></p>`
	if err := app.enforceDLP(acc, &v); err == nil {
		t.Fatal("HTML attribute content bypassed DLP")
	}
}

func TestHTMLDraftEmptyBodyCannotSendStaleFallback(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	draft, err := app.CreateDraft(ctx, CreateDraftInput{
		AccountID: acc.ID, To: []string{"alice@corp.local"}, Subject: "test",
		Body: "stale fallback", BodyHTML: "<script>alert(1)</script>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.PreviewSend(ctx, draft.Draft.ID); err == nil {
		t.Fatal("HTML removed by sanitization must not send an old fallback")
	}
	html := "<p>Actual content</p>"
	if _, err := app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, BodyHTML: &html}); err != nil {
		t.Fatal(err)
	}
	empty := ""
	draft, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, BodyHTML: &empty})
	if err != nil || draft.Version.BodyHTML != "" || draft.Version.BodyText != "" {
		t.Fatalf("clearing HTML must also clear its derived alternative: %+v, %v", draft, err)
	}
	if _, err := app.PreviewSend(ctx, draft.Draft.ID); err == nil {
		t.Fatal("empty HTML draft must not be sendable")
	}
	plain := "Explicit plain replacement"
	draft, err = app.UpdateDraft(ctx, UpdateDraftInput{DraftID: draft.Draft.ID, BodyHTML: &empty, Body: &plain})
	if err != nil || draft.Version.BodyText != plain || draft.Version.BodyHTML != "" {
		t.Fatalf("switching to an explicit plain replacement failed: %+v, %v", draft, err)
	}
}

func TestHTMLLegacyDraftPreviewMatchesSanitizedWireWithoutChangingApproval(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	app.Cfg.Send.DLPPolicy = "block"
	app.Cfg.Send.DLPKeywords = []string{"confidential"}
	draft, err := app.CreateDraft(ctx, CreateDraftInput{
		AccountID: acc.ID, To: []string{"alice@external.test"}, Subject: "Legacy HTML", Body: "old body",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Import a version directly, as an older application or migration could.
	legacy := draft.Version
	legacy.BodyText = "confidential stale plain alternative"
	legacy.BodyHTML = `<p style="color:#2563eb" onclick="sendSecret()">Actual <strong>message</strong></p><script>confidential script</script><img src="https://tracker.invalid/pixel">`
	if _, err := app.Store.AddDraftVersion(ctx, userIDFrom(ctx), draft.Draft.ID, &legacy); err != nil {
		t.Fatal(err)
	}
	stored, err := app.GetDraft(ctx, draft.Draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	preview, token, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Body != "Actual message" || preview.DLPBlocked || strings.Contains(preview.BodyHTML, "onclick") || strings.Contains(preview.BodyHTML, "<script") || strings.Contains(preview.BodyHTML, "<img") {
		t.Fatalf("preview must expose only the canonical safe bodies: %+v", preview)
	}
	if preview.PayloadHash != sendPayloadHash(acc, &stored.Version) {
		t.Fatal("normalizing legacy HTML unexpectedly changed the stored approval payload")
	}
	out, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token})
	if err != nil || out.Status != domain.OutboundSent || len(smtp.sent) != 1 {
		t.Fatalf("canonical legacy message failed to send: %+v, %v", out, err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(smtp.sent[0].raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	parts := multipart.NewReader(msg.Body, params["boundary"])
	for _, want := range []string{preview.Body, preview.BodyHTML} {
		part, err := parts.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(part)
		if err != nil || strings.ReplaceAll(string(body), "\r\n", "\n") != want {
			t.Fatalf("preview/wire divergence: got %q, preview %q (%v)", body, want, err)
		}
	}
	legacy.BodyHTML = "<script>removed</script>"
	if err := validateDraftForSend(acc, &legacy); err == nil {
		t.Fatal("legacy unsafe-only HTML must not fall back to stale plain text")
	}
}
