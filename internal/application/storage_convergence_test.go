package application_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/adapters/pgstore"
	"postra/internal/adapters/secretstore"
	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
)

type convergenceStorage interface {
	application.Storage
	Close() error
	EnableEncryption(*crypto.KEK)
}

// POSTRA_TEST_PG must identify an isolated test database. This test writes only
// uniquely named users/resources but intentionally persists administrator
// settings to verify restart semantics. Never point it at a production DB.
func TestStorageConvergenceRestartAndImmutableApproval(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			cfg := config.Default()
			cfg.DataDir, cfg.AllowInsecureMail, cfg.AllowPrivateHosts = t.TempDir(), true, true
			cfg.StorageDriver = backend
			if backend == "postgres" {
				cfg.PostgresDSN = os.Getenv("POSTRA_TEST_PG")
				if cfg.PostgresDSN == "" {
					t.Skip("set POSTRA_TEST_PG to an isolated pgvector test database")
				}
			}
			open := func() convergenceStorage {
				t.Helper()
				if backend == "postgres" {
					store, err := pgstore.Open(context.Background(), cfg.PostgresDSN)
					if err != nil {
						t.Fatal(err)
					}
					return store
				}
				store, err := persistence.Open(filepath.Join(cfg.DataDir, "contracts.db"))
				if err != nil {
					t.Fatal(err)
				}
				return store
			}
			store := open()
			smtp := &storageContractSMTP{}
			start := func(store convergenceStorage) *application.App {
				t.Helper()
				kek, err := crypto.LoadOrCreateKEK(cfg.DataDir)
				if err != nil {
					t.Fatal(err)
				}
				store.EnableEncryption(kek)
				objects := objectstore.NewEncrypted(objectstore.NewDB(store, nil), kek)
				app, err := application.New(cfg, store, objects, secretstore.NewDB(store, kek), nil, smtp, nil)
				if err != nil {
					t.Fatal(err)
				}
				return app
			}
			app := start(store)
			t.Cleanup(func() { app.Shutdown(); _ = store.Close() })
			ownerID := persistence.NewID("contract_owner")
			user := &domain.User{ID: ownerID, LoginID: ownerID, Email: ownerID + "@corp.local", Role: domain.RoleAdmin, Status: domain.UserActive, AuthProvider: "local"}
			ctx := application.WithPrincipal(context.Background(), domain.Principal{UserID: ownerID, LoginID: ownerID, Role: domain.RoleAdmin, AuthMethod: "local"})
			if err := store.CreateUser(ctx, user, ""); err != nil {
				t.Fatal(err)
			}
			account := &domain.MailAccount{ID: persistence.NewID("acc"), UserID: ownerID, Name: "계약 검증", Email: user.Email, Status: domain.AccountActive, SMTPHost: "127.0.0.1", SMTPPort: 25, SMTPSecurity: domain.SecurityNone, SMTPAuth: "none"}
			if err := store.CreateAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			messages := []string{persistence.NewID("msg"), persistence.NewID("msg")}
			for i, id := range messages {
				message := &domain.Message{ID: id, UserID: ownerID, AccountID: account.ID, UIDL: id, Subject: "읽음·업무 계약", RawHash: id, Date: time.Now().Unix(), CreatedAt: time.Now().Unix()}
				if err := store.InsertMessage(ctx, message, &domain.MessageBody{MessageID: id, TextBody: "encrypted body"}, nil); err != nil {
					t.Fatal(err)
				}
				status := "pending"
				if i == 1 {
					status = "in_progress"
				}
				if _, err := app.SetMessageWorkStatus(ctx, id, status); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := app.BatchUpdateMessages(ctx, application.BatchUpdateOptions{MessageIDs: []string{messages[0]}, Action: application.BatchActionMarkRead}); err != nil || result.Succeeded != 1 {
				t.Fatalf("mark read: %+v %v", result, err)
			}
			key, rawKey, err := app.CreateMCPKeyWithScopes(ctx, "contract key", []string{"mail.read", "mail.draft"})
			if err != nil {
				t.Fatal(err)
			}
			signature, err := app.SaveMailSignature(ctx, application.SaveMailSignatureInput{Name: "재시작 서명", AccountID: account.ID, DisplayName: "홍길동", Title: "담당", Department: "개발", Company: "사내", Email: user.Email, BodyHTML: "<p>계약 서명</p>", Format: "html"})
			if err != nil {
				t.Fatal(err)
			}
			const secretValue = "fixture-api-key-persisted-encrypted"
			address := "127.0.0.1:9191"
			if app.Cfg.HTTPAddr == address {
				address = "127.0.0.1:9192"
			}
			view, err := app.AdminPatchSettings(ctx, application.SettingsPatch{Values: map[string]string{"sync.auto_sync_minutes": "7", "system.http_addr": address}, Secrets: map[string]string{"ai.api_key_ref": secretValue}})
			if err != nil {
				t.Fatal(err)
			}
			pending := false
			for _, field := range view.Fields {
				if field.Key == "system.http_addr" {
					pending = field.PendingRestart
				}
			}
			if app.EffectiveConfig().HTTPAddr == address || !pending {
				t.Fatal("restart setting applied before restart")
			}
			draft, err := app.CreateDraft(ctx, application.CreateDraftInput{AccountID: account.ID, To: []string{"colleague@corp.local"}, Subject: "첨부 승인 계약", BodyHTML: "<p>발송할 본문</p>", Format: "html"})
			if err != nil {
				t.Fatal(err)
			}
			_, oldApproval, err := app.RequestSendApproval(ctx, draft.Draft.ID, "contract", 120)
			if err != nil {
				t.Fatal(err)
			}
			initialVersion := draft.Version.Version
			// Each invocation has its own temporary KEK. A unique object avoids
			// deduplicating against ciphertext from an earlier test invocation.
			fileBody := "immutable attachment fixture " + ownerID
			draft, err = app.AddDraftAttachment(ctx, application.AddDraftAttachmentInput{DraftID: draft.Draft.ID, Name: "계약.txt", DataBase64: base64.StdEncoding.EncodeToString([]byte(fileBody))})
			if err != nil {
				t.Fatal(err)
			}
			attachmentID := draft.Version.Attachments[0].ID
			if _, err := app.Send(ctx, application.SendInput{DraftID: draft.Draft.ID, ApprovalToken: oldApproval.Token}); err == nil || len(smtp.sent) != 0 {
				t.Fatal("stale approval authorized attachment change")
			}

			// A new connection, KEK instance, object adapter, secret adapter and
			// application exercise durable state instead of in-memory caches.
			app.Shutdown()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = open()
			app = start(store)
			if app.EffectiveConfig().HTTPAddr != address || app.EffectiveConfig().Sync.AutoSyncMinutes != 7 {
				t.Fatal("configuration restart diverged")
			}
			handle, err := app.Secrets.Acquire(ctx, domain.SecretRef(app.Cfg.AI.APIKeyRef), domain.PurposeAIKey)
			if err != nil {
				t.Fatal(err)
			}
			valid := string(handle.Reveal()) == secretValue
			handle.Zero()
			if !valid {
				t.Fatal("encrypted API key did not survive reopen")
			}
			storedSignature, err := app.GetMailSignature(ctx, signature.ID)
			if err != nil || storedSignature.DisplayName != "홍길동" || storedSignature.Department != "개발" || !strings.Contains(storedSignature.BodyHTML, "계약 서명") {
				t.Fatalf("signature profile round-trip: %+v %v", storedSignature, err)
			}
			loadedKey, principal, err := app.AuthenticateMCPKey(ctx, rawKey)
			if err != nil || loadedKey.LegacyScopes || len(principal.MCPScopes) != 2 {
				t.Fatalf("scope round-trip: %+v %v", loadedKey, err)
			}
			if _, err := app.UpdateMCPKeyScopes(ctx, key.ID, []string{}, false); err != nil {
				t.Fatal(err)
			}
			loadedKey, _, err = app.AuthenticateMCPKey(ctx, rawKey)
			if err != nil || len(loadedKey.Scopes) != 0 || loadedKey.LegacyScopes {
				t.Fatal("explicit empty scopes became legacy defaults")
			}
			message, err := app.GetMessage(ctx, messages[0], true)
			if err != nil || !message.Message.IsRead || message.Body.TextBody != "encrypted body" {
				t.Fatalf("read/body round-trip: %+v %v", message, err)
			}
			for _, status := range []string{"pending", "in_progress"} {
				rows, err := app.TeamInbox(ctx, status, "", 100)
				if err != nil || len(rows) != 2 {
					t.Fatalf("work aliases %s: %d %v", status, len(rows), err)
				}
			}
			oldVersion, err := store.GetDraftVersion(ctx, ownerID, draft.Draft.ID, initialVersion)
			if err != nil || len(oldVersion.Attachments) != 0 {
				t.Fatal("historical approval version mutated")
			}
			_, file, err := app.GetDraftAttachment(ctx, draft.Draft.ID, attachmentID, 0)
			if err != nil || string(file) != fileBody {
				t.Fatalf("encrypted attachment round-trip: %v", err)
			}
			foreign := application.WithPrincipal(context.Background(), domain.Principal{UserID: "not-the-owner", Role: domain.RoleAdmin})
			if _, _, err := app.GetDraftAttachment(foreign, draft.Draft.ID, attachmentID, 0); err == nil {
				t.Fatal("admin crossed attachment owner boundary")
			}
			preview, approval, err := app.RequestSendApproval(ctx, draft.Draft.ID, "contract", 120)
			if err != nil || len(preview.Attachments) != 1 {
				t.Fatalf("preview attachment lost: %+v %v", preview, err)
			}
			sent, err := app.Send(ctx, application.SendInput{DraftID: draft.Draft.ID, ApprovalToken: approval.Token, IdempotencyKey: "contract-" + draft.Draft.ID})
			if err != nil || sent.Status != domain.OutboundSent || len(smtp.sent) != 1 {
				t.Fatalf("send diverged: %+v %v", sent, err)
			}
			if _, err := app.Send(ctx, application.SendInput{DraftID: draft.Draft.ID, ApprovalToken: approval.Token, IdempotencyKey: "contract-" + draft.Draft.ID}); err != nil || len(smtp.sent) != 1 {
				t.Fatal("idempotent replay sent duplicate mail")
			}
			wire, err := mail.ReadMessage(bytes.NewReader(smtp.sent[0].raw))
			if err != nil {
				t.Fatal(err)
			}
			mediaType, params, err := mime.ParseMediaType(wire.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/mixed" {
				t.Fatalf("expected mixed MIME: %s %v", mediaType, err)
			}
			parts, found := multipart.NewReader(wire.Body, params["boundary"]), false
			for {
				part, err := parts.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(part.Header.Get("Content-Disposition"), "attachment") {
					continue
				}
				body, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
				if err != nil {
					t.Fatal(err)
				}
				if string(body) == fileBody {
					found = true
				}
			}
			if !found {
				t.Fatal("approved attachment not present in SMTP MIME")
			}
		})
	}
}

type storageContractSMTP struct{ sent []struct{ raw []byte } }

func (s *storageContractSMTP) TestConnection(context.Context, domain.SMTPSendOptions) (*domain.ConnDiagnostics, error) {
	return &domain.ConnDiagnostics{OK: true}, nil
}
func (s *storageContractSMTP) Send(_ context.Context, _ domain.SMTPSendOptions, _ domain.Envelope, reader io.Reader) (domain.SendReceipt, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return domain.SendReceipt{}, err
	}
	s.sent = append(s.sent, struct{ raw []byte }{data})
	return domain.SendReceipt{ServerResponse: "250 fixture accepted"}, nil
}
