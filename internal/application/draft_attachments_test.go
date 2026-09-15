package application

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"

	"postra/internal/domain"
)

func attachmentImage(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestDraftAttachmentsApprovalNestedMIMEAndIsolation(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	draft, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, To: []string{"a@corp.local"}, Subject: "첨부 테스트", Body: "보고 내용", Format: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	_, oldToken, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	picture := attachmentImage(t)
	draft, err = app.AddDraftAttachment(ctx, AddDraftAttachmentInput{DraftID: draft.Draft.ID, Name: "그림.png", DataBase64: base64.StdEncoding.EncodeToString(picture), Inline: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Version.Attachments) != 1 || !strings.Contains(draft.Version.BodyHTML, "cid:postra-att_") {
		t.Fatalf("CID not inserted: %+v", draft.Version)
	}
	draft, err = app.AddDraftAttachment(ctx, AddDraftAttachmentInput{DraftID: draft.Draft.ID, Name: "진행 보고.txt", DataBase64: base64.StdEncoding.EncodeToString([]byte("첨부 본문"))})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := app.GetDraft(ctx, draft.Draft.ID)
	if err != nil || len(loaded.Version.Attachments) != 2 {
		t.Fatalf("attachment persistence: %+v %v", loaded, err)
	}
	if _, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: oldToken.Token}); err == nil {
		t.Fatal("upload did not invalidate old approval")
	}
	other := WithPrincipal(ctx, domain.Principal{UserID: "not-the-owner", Role: domain.RoleAdmin})
	if _, _, err := app.GetDraftAttachment(other, draft.Draft.ID, draft.Version.Attachments[0].ID, 0); err == nil {
		t.Fatal("admin could download another user's attachment")
	}
	preview, token, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Attachments) != 2 {
		t.Fatal("preview omitted attachments")
	}
	sent, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token})
	if err != nil || sent.Status != domain.OutboundSent {
		t.Fatalf("send: %+v %v", sent, err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(smtp.sent[0].raw))
	if err != nil {
		t.Fatal(err)
	}
	types := []string{}
	foundImage, foundFile := false, false
	var walk func(string, io.Reader)
	walk = func(rawType string, reader io.Reader) {
		media, params, err := mime.ParseMediaType(rawType)
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, media)
		if !strings.HasPrefix(media, "multipart/") {
			return
		}
		parts := multipart.NewReader(reader, params["boundary"])
		for {
			part, err := parts.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if part.Header.Get("Content-ID") != "" {
				bytes, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
				if err != nil || !bytesEqual(bytes, picture) {
					t.Fatal("inline image changed")
				}
				foundImage = true
				continue
			}
			_, disposition, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if disposition["filename"] == "진행 보고.txt" {
				data, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
				if string(data) != "첨부 본문" {
					t.Fatal("attachment bytes changed")
				}
				foundFile = true
				continue
			}
			walk(part.Header.Get("Content-Type"), part)
		}
	}
	walk(message.Header.Get("Content-Type"), message.Body)
	if !foundImage || !foundFile || strings.Join(types, ",") != "multipart/mixed,multipart/related,multipart/alternative,text/plain,text/html" {
		t.Fatalf("MIME hierarchy mismatch: %v image=%v file=%v", types, foundImage, foundFile)
	}
}

func bytesEqual(a, b []byte) bool { return bytes.Equal(a, b) }

func TestAttachmentRemovalPolicyDLPAndContentIntegrity(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	acc := mustAccount(t, app)
	draft, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, To: []string{"external@example.com"}, Subject: "첨부", Body: "일반 본문", Format: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AddDraftAttachment(ctx, AddDraftAttachmentInput{DraftID: draft.Draft.ID, Name: "run.exe", DataBase64: base64.StdEncoding.EncodeToString([]byte("MZ executable"))}); err == nil {
		t.Fatal("dangerous extension accepted")
	}
	if _, err := app.AddDraftAttachment(ctx, AddDraftAttachmentInput{DraftID: draft.Draft.ID, Name: "fake.png", DataBase64: base64.StdEncoding.EncodeToString([]byte("not an image")), Inline: true}); err == nil {
		t.Fatal("fake inline image accepted")
	}
	draft, err = app.AddDraftAttachment(ctx, AddDraftAttachmentInput{DraftID: draft.Draft.ID, Name: "secret.txt", DataBase64: base64.StdEncoding.EncodeToString([]byte("confidential attachment"))})
	if err != nil {
		t.Fatal(err)
	}
	app.Cfg.Send.DLPPolicy = "block"
	app.Cfg.Send.DLPKeywords = []string{"confidential"}
	preview, token, err := app.RequestSendApproval(ctx, draft.Draft.ID, "test", 60)
	if err != nil || !preview.DLPBlocked {
		t.Fatalf("attachment text bypassed DLP: %+v %v", preview, err)
	}
	out, err := app.Send(ctx, SendInput{DraftID: draft.Draft.ID, ApprovalToken: token.Token})
	if len(smtp.sent) != 0 || err == nil && out.Status == domain.OutboundSent {
		t.Fatalf("DLP attachment sent: %+v %v", out, err)
	}
	attachment := draft.Version.Attachments[0]
	attachment.Hash = strings.Repeat("0", 64)
	if _, err := app.readDraftAttachment(ctx, attachment); err == nil {
		t.Fatal("tampered attachment hash accepted")
	}
	previous := draft.Version.Version
	draft, err = app.RemoveDraftAttachment(ctx, draft.Draft.ID, attachment.ID)
	if err != nil || len(draft.Version.Attachments) != 0 || draft.Version.Version <= previous {
		t.Fatalf("remove did not create a new version: %+v %v", draft, err)
	}
	if _, data, err := app.GetDraftAttachment(ctx, draft.Draft.ID, attachment.ID, previous); err != nil || len(data) == 0 {
		t.Fatalf("historical attachment was physically deleted: %v", err)
	}
}
