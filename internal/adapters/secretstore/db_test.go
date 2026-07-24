package secretstore

import (
	"context"
	"path/filepath"
	"testing"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/crypto"
)

func kekFromRing(t *testing.T) *crypto.KEK {
	t.Helper()
	ring, err := crypto.NewRandomKeyringJSON()
	if err != nil {
		t.Fatal(err)
	}
	kek, err := crypto.KEKFromKeyringJSON(ring)
	if err != nil {
		t.Fatal(err)
	}
	return kek
}

// A secret stored through the DB-backed store must still be acquirable after a
// "restart" — a brand-new store + secret-store instance over the same database
// and the same (shared) KEK. This is the regression the K8s/Postgres
// deployment hit: the value lived in a per-pod file that vanished on restart.
func TestDBSecretSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "postra.db")
	ctx := context.Background()

	kek := kekFromRing(t)
	ringJSON, _ := kek.KeyringJSON()

	store, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ss := NewDB(store, kek)
	ref, err := ss.Put(ctx, domain.PutSecretRequest{
		OwnerUserID: "u1", Type: domain.SecretMailPassword, Provider: "local",
		Value: domain.NewSecretHandle([]byte("hunter2")), Label: "IMAP",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	// Simulate a pod restart: reopen the same DB, reconstruct the same shared
	// KEK, and acquire the secret through a fresh store instance.
	store2, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	kek2, err := crypto.KEKFromKeyringJSON(ringJSON)
	if err != nil {
		t.Fatal(err)
	}
	ss2 := NewDB(store2, kek2)
	h, err := ss2.Acquire(ctx, ref, domain.PurposePOP3Auth)
	if err != nil {
		t.Fatalf("secret lost across restart: %v", err)
	}
	if string(h.Reveal()) != "hunter2" {
		t.Fatalf("got %q, want hunter2", h.Reveal())
	}
	h.Zero()
}

// A different KEK on restart (the old per-pod ephemeral-keyring behavior) must
// fail cleanly rather than silently returning garbage — this is exactly the
// "secret_acquire" diagnostic error, and why the shared keyring is required.
func TestDBSecretRejectsChangedKEK(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "postra.db")
	ctx := context.Background()

	store, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ss := NewDB(store, kekFromRing(t))
	ref, err := ss.Put(ctx, domain.PutSecretRequest{
		OwnerUserID: "u1", Type: domain.SecretMailPassword,
		Value: domain.NewSecretHandle([]byte("hunter2")), Label: "IMAP",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fresh, unrelated KEK — decryption must fail.
	ss2 := NewDB(store, kekFromRing(t))
	if _, err := ss2.Acquire(ctx, ref, domain.PurposePOP3Auth); err == nil {
		t.Fatal("expected acquire to fail with a changed KEK, got nil error")
	}
}

// Rotate self-heals a ref whose value was never stored (or was lost), so a
// re-entered password sticks even if the credential_ref predates the value.
func TestDBSecretRotateSelfHeals(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store, err := persistence.Open(filepath.Join(dir, "postra.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	kek := kekFromRing(t)
	ss := NewDB(store, kek)
	const ref = domain.SecretRef("sec_orphaned")
	if err := ss.Rotate(ctx, ref, domain.RotateSecretRequest{Value: domain.NewSecretHandle([]byte("fresh"))}); err != nil {
		t.Fatal(err)
	}
	h, err := ss.Acquire(ctx, ref, domain.PurposePOP3Auth)
	if err != nil {
		t.Fatal(err)
	}
	if string(h.Reveal()) != "fresh" {
		t.Fatalf("got %q, want fresh", h.Reveal())
	}
	h.Zero()
}
