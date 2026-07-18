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

func TestKEKPersistence(t *testing.T) {
	dir := t.TempDir()
	k1, _ := LoadOrCreateKEK(dir)
	env, _ := k1.Encrypt([]byte("v"), []byte("a"))
	k2, _ := LoadOrCreateKEK(dir) // reload from file
	if _, err := k2.Decrypt(env, []byte("a")); err != nil {
		t.Fatalf("reloaded KEK cannot decrypt: %v", err)
	}
}
