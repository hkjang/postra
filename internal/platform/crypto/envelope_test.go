package crypto

import (
	"bytes"
	"testing"
)

func TestEnvelopeRoundtrip(t *testing.T) {
	kek, err := LoadOrCreateKEK(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("user:acct:type")
	env, err := kek.Encrypt([]byte("s3cret-password"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env.Ciphertext, []byte("s3cret")) {
		t.Fatal("ciphertext leaks plaintext")
	}
	pt, err := kek.Decrypt(env, aad)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "s3cret-password" {
		t.Fatalf("roundtrip mismatch: %q", pt)
	}
}

func TestEnvelopeWrongAAD(t *testing.T) {
	kek, _ := LoadOrCreateKEK(t.TempDir())
	env, _ := kek.Encrypt([]byte("v"), []byte("aad-1"))
	if _, err := kek.Decrypt(env, []byte("aad-2")); err == nil {
		t.Fatal("decrypt with wrong AAD must fail")
	}
}

func TestEnvelopeTamper(t *testing.T) {
	kek, _ := LoadOrCreateKEK(t.TempDir())
	env, _ := kek.Encrypt([]byte("value"), []byte("a"))
	env.Ciphertext[0] ^= 0xff
	if _, err := kek.Decrypt(env, []byte("a")); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestKEKRotationAndRewrap(t *testing.T) {
	dir := t.TempDir()
	kek, _ := LoadOrCreateKEK(dir)
	aad := []byte("ctx")
	env, err := kek.Encrypt([]byte("secret-value"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if env.KeyVersion != 1 {
		t.Fatalf("initial version = %d, want 1", env.KeyVersion)
	}

	// Rotate: old envelope still decrypts (old key retained).
	nv, err := kek.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if nv != 2 || kek.CurrentVersion() != 2 {
		t.Fatalf("rotate gave v%d, current %d", nv, kek.CurrentVersion())
	}
	if pt, err := kek.Decrypt(env, aad); err != nil || string(pt) != "secret-value" {
		t.Fatalf("old envelope must still decrypt after rotate: %v %q", err, pt)
	}
	// New encryption uses the new version.
	env2, _ := kek.Encrypt([]byte("x"), aad)
	if env2.KeyVersion != 2 {
		t.Fatalf("new envelope version = %d, want 2", env2.KeyVersion)
	}

	// Rewrap the old envelope to the current version.
	changed, err := kek.Rewrap(env, aad)
	if err != nil || !changed {
		t.Fatalf("rewrap: changed=%v err=%v", changed, err)
	}
	if env.KeyVersion != 2 {
		t.Fatalf("rewrapped version = %d, want 2", env.KeyVersion)
	}
	if pt, err := kek.Decrypt(env, aad); err != nil || string(pt) != "secret-value" {
		t.Fatalf("rewrapped envelope must decrypt: %v %q", err, pt)
	}

	// After retiring v1, the rewrapped envelope still decrypts (it's on v2).
	if err := kek.RetireVersion(1); err != nil {
		t.Fatal(err)
	}
	if _, err := kek.Decrypt(env, aad); err != nil {
		t.Fatalf("rewrapped envelope must survive v1 retirement: %v", err)
	}
	// A non-rewrapped v1 envelope becomes undecryptable after retirement.
	freshV1 := &Envelope{}
	*freshV1 = *env2
	freshV1.KeyVersion = 1
	if _, err := kek.Decrypt(freshV1, aad); err == nil {
		t.Fatal("v1 envelope must fail after v1 is retired")
	}

	// Persistence: reload keyring and confirm both versions available.
	reloaded, err := LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CurrentVersion() != 2 {
		t.Fatalf("reloaded current = %d, want 2", reloaded.CurrentVersion())
	}
}

func TestKEKPersistence(t *testing.T) {
	dir := t.TempDir()
	k1, _ := LoadOrCreateKEK(dir)
	env, _ := k1.Encrypt([]byte("v"), []byte("a"))
	k2, _ := LoadOrCreateKEK(dir) // reload from file
	if _, err := k2.Decrypt(env, []byte("a")); err != nil {
		t.Fatalf("reloaded KEK cannot decrypt: %v", err)
	}
}
