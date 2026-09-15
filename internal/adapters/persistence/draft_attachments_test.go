package persistence

import (
	"context"
	"path/filepath"
	"testing"

	"postra/internal/domain"
)

func TestDraftAttachmentsPersistByVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.EnsureUser(ctx, "owner", "owner"); err != nil {
		t.Fatal(err)
	}
	account := &domain.MailAccount{ID: "acc", UserID: "owner", Name: "account", Email: "owner@test.local", Status: domain.AccountActive}
	if err := store.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	draft := &domain.Draft{ID: "draft", UserID: "owner", AccountID: account.ID, Kind: domain.DraftNew, Status: domain.DraftOpen}
	attachment := domain.DraftAttachment{ID: "att", Name: "보고서.pdf", MIMEType: "application/pdf", Size: 12, Hash: "content-hash", StorageURI: "objects/private/report", ContentID: "report", ScanStatus: domain.ScanClean}
	version := &domain.DraftVersion{Subject: "Subject", BodyText: "Body", Author: "user", Attachments: []domain.DraftAttachment{attachment}}
	if err := store.CreateDraft(ctx, draft, version); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetDraftVersion(ctx, "owner", draft.ID, 1)
	if err != nil || len(got.Attachments) != 1 || got.Attachments[0] != attachment {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	if _, err := store.GetDraftVersion(ctx, "other", draft.ID, 1); err == nil {
		t.Fatal("attachment crossed owner boundary")
	}
	got.Attachments = nil
	if _, err := store.AddDraftVersion(ctx, "owner", draft.ID, got); err != nil {
		t.Fatal(err)
	}
	original, _ := store.GetDraftVersion(ctx, "owner", draft.ID, 1)
	if len(original.Attachments) != 1 {
		t.Fatal("edit mutated previous approved attachment version")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, current, err := reopened.GetDraft(ctx, "owner", draft.ID)
	if err != nil || len(current.Attachments) != 0 {
		t.Fatalf("restart current version: %+v %v", current, err)
	}
}
