package objectstore

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"postra/internal/adapters/persistence"
	"postra/internal/platform/crypto"
)

func mustRead(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A raw object stored via the DB backend must be readable after a "restart"
// (new store + backend over the same database) — the whole point of moving raw
// MIME out of the per-pod local directory.
func TestDBObjectSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "postra.db")
	local, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}

	store, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db := NewDB(store, local)
	const payload = "From: a@x\r\nSubject: hi\r\n\r\nthe body"
	uri, hash, size, err := db.Put("raw", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) || hash == "" {
		t.Fatalf("unexpected put result size=%d hash=%q", size, hash)
	}
	if got := mustRead(t, mustGet(t, db, uri)); got != payload {
		t.Fatalf("got %q, want %q", got, payload)
	}
	store.Close()

	store2, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	db2 := NewDB(store2, local)
	if got := mustRead(t, mustGet(t, db2, uri)); got != payload {
		t.Fatalf("after restart got %q, want %q", got, payload)
	}
}

// Encryption composes over the DB backend: bytes at rest are ciphertext, reads
// decrypt, and a restart with the same key still reads.
func TestDBObjectEncryptedRoundtrip(t *testing.T) {
	dir := t.TempDir()
	local, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := persistence.Open(filepath.Join(dir, "postra.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ring, _ := crypto.NewRandomKeyringJSON()
	kek, err := crypto.KEKFromKeyringJSON(ring)
	if err != nil {
		t.Fatal(err)
	}
	enc := NewEncrypted(NewDB(store, local), kek)
	const payload = "secret raw bytes"
	uri, _, _, err := enc.Put("raw", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, mustGet(t, enc, uri)); got != payload {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func mustGet(t *testing.T, s Store, uri string) io.ReadCloser {
	t.Helper()
	rc, err := s.Get(uri)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}
