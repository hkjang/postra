package persistence

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"postra/internal/domain"
)

func TestMCPScopesMigrationAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scope-migration.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureUser(ctx, "legacy-owner", "legacy-owner"); err != nil {
		t.Fatal(err)
	}
	// This empty scopes_json value models a row written before the additive
	// migration. NULL/[] in new rows are intentionally not legacy permissions.
	_, err = store.db.ExecContext(ctx, `INSERT INTO mcp_keys (id,user_id,name,key_hash,key_prefix,status,created_at) VALUES ('legacy','legacy-owner','legacy','hash-legacy','mk_x','active',1)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key, _, err := store.GetMCPKeyByHash(ctx, "hash-legacy")
	if err != nil || !key.LegacyScopes || !slices.Contains(key.Scopes, "mail.send") || slices.Contains(key.Scopes, "admin.write") {
		t.Fatalf("legacy compatibility: %+v %v", key, err)
	}
	if err := store.UpdateMCPKeyScopes(ctx, "someone-else", key.ID, []string{}); err != domain.ErrNotFound {
		t.Fatalf("wrong owner could edit scopes: %v", err)
	}
	if err := store.UpdateMCPKeyScopes(ctx, "legacy-owner", key.ID, []string{}); err != nil {
		t.Fatal(err)
	}
	key, _, err = store.GetMCPKeyByHash(ctx, "hash-legacy")
	if err != nil || key.LegacyScopes || len(key.Scopes) != 0 {
		t.Fatalf("deny-all did not persist: %+v %v", key, err)
	}
	if err := store.CreateMCPKey(ctx, &domain.MCPKey{ID: "new", UserID: "legacy-owner", Name: "new", KeyHash: "hash-new", KeyPrefix: "mk_y", Status: "active", Scopes: []string{"mail.read"}}); err != nil {
		t.Fatal(err)
	}
	keys, err := store.ListAllMCPKeys(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("scope list: %+v %v", keys, err)
	}
}
