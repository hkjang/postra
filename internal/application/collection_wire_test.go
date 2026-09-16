package application_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"postra/internal/adapters/persistence"
	"postra/internal/adapters/pgstore"
	"postra/internal/application"
	"postra/internal/domain"
)

// The stores may return nil slices internally; shared application services
// guarantee [] on successful collection responses for both REST and MCP.
func TestEmptyCollectionWireContractsSQLiteAndPostgres(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store application.Storage
			if backend == "postgres" {
				dsn := os.Getenv("POSTRA_TEST_PG")
				if dsn == "" {
					t.Skip("set POSTRA_TEST_PG to an isolated test database")
				}
				pg, err := pgstore.Open(context.Background(), dsn)
				if err != nil {
					t.Fatal(err)
				}
				store = pg
				t.Cleanup(func() { _ = pg.Close() })
			} else {
				sqlite, err := persistence.Open(filepath.Join(t.TempDir(), "collections.db"))
				if err != nil {
					t.Fatal(err)
				}
				store = sqlite
				t.Cleanup(func() { _ = sqlite.Close() })
			}
			owner := persistence.NewID("collections_owner")
			ctx := application.WithPrincipal(context.Background(), domain.Principal{UserID: owner, Role: domain.RoleAdmin, AuthMethod: "local"})
			if err := store.EnsureUser(ctx, owner, owner); err != nil {
				t.Fatal(err)
			}
			app := &application.App{Store: store}
			assertJSON := func(name string, result any, err error, expected string) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				raw, err := json.Marshal(result)
				if err != nil || !strings.Contains(string(raw), expected) {
					t.Fatalf("%s empty contract: %s (%v), want %s", name, raw, err, expected)
				}
			}
			rawRows, err := store.ListActionCards(ctx, owner, "", 100)
			if err != nil || len(rawRows) != 0 {
				t.Fatal("new owner unexpectedly had action cards")
			}
			rawStorage, _ := json.Marshal(map[string]any{"cards": rawRows})
			cards, err := app.ListActionCards(ctx, "", 100)
			assertJSON("actions", map[string]any{"cards": cards}, err, `"cards":[]`)
			wire, _ := json.Marshal(map[string]any{"cards": cards})
			t.Logf("%s empty storage=%s shared-service=%s", backend, rawStorage, wire)
			accounts, err := app.ListAccounts(ctx)
			assertJSON("accounts", accounts, err, `[]`)
			rules, err := app.ListRules(ctx)
			assertJSON("rules", rules, err, `[]`)
			jobs, err := app.ListJobs(ctx, 100)
			assertJSON("jobs", jobs, err, `[]`)
			keys, err := app.ListMyMCPKeys(ctx)
			assertJSON("keys", keys, err, `[]`)
			audit, err := app.SearchAudit(ctx, 100)
			assertJSON("audit", audit, err, `[]`)
			search, err := app.Search(ctx, domain.SearchQuery{})
			assertJSON("search", search, err, `"messages":[]`)
			drafts, err := app.ListDrafts(ctx, "", 100, "")
			assertJSON("drafts", drafts, err, `"drafts":[]`)
			outbound, err := app.ListOutbound(ctx, 100)
			assertJSON("outbound", outbound, err, `[]`)
			team, err := app.TeamInbox(ctx, "", "", 100)
			assertJSON("team", team, err, `[]`)
			work, err := app.WorkInbox(ctx, "", 100)
			for _, bucket := range []string{"important", "snoozed_due", "attention", "reference"} {
				assertJSON("work inbox "+bucket, work, err, `"`+bucket+`":[]`)
			}

			account := &domain.MailAccount{ID: persistence.NewID("acc"), UserID: owner, Email: "owner@corp.local", Status: domain.AccountActive}
			if err := store.CreateAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			message := &domain.Message{ID: persistence.NewID("msg"), UserID: owner, AccountID: account.ID, UIDL: "collection-source", RawHash: owner, Subject: "source"}
			if err := store.InsertMessage(ctx, message, nil, nil); err != nil {
				t.Fatal(err)
			}
			attachments, err := app.ListAttachments(ctx, message.ID)
			assertJSON("attachments", attachments, err, `[]`)
			collab, err := app.GetMessageCollab(ctx, message.ID)
			assertJSON("notes", collab, err, `"notes":[]`)
			view, err := app.GetMessage(ctx, message.ID, false)
			if err != nil || view.Body != nil {
				t.Fatal("an unloaded optional body must remain absent")
			}
			card := &domain.ActionCard{ID: persistence.NewID("act"), UserID: owner, MessageID: message.ID, Type: "todo", Title: "private action", Status: domain.ActionCardPending}
			if err := store.CreateActionCard(ctx, card); err != nil {
				t.Fatal(err)
			}
			cards, err = app.ListActionCards(ctx, domain.ActionCardDone, 100)
			assertJSON("filtered actions", map[string]any{"cards": cards}, err, `"cards":[]`)
			other := application.WithPrincipal(context.Background(), domain.Principal{UserID: persistence.NewID("other"), Role: domain.RoleAdmin, AuthMethod: "local"})
			cards, err = app.ListActionCards(other, "", 100)
			assertJSON("other owner actions", map[string]any{"cards": cards}, err, `"cards":[]`)
		})
	}
}
