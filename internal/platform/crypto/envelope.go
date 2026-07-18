// Package crypto implements envelope encryption (KEK -> DEK -> value)
// with AES-256-GCM, used by the local secret store and object encryption.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const keySize = 32

// Envelope is the persisted form of an encrypted value. It never contains
// key material in plaintext: DEK is wrapped by the KEK.
type Envelope struct {
	Alg        string `json:"alg"` // "aes-256-gcm"
	KeyVersion int    `json:"key_version"`
	WrappedDEK []byte `json:"wrapped_dek"`
	DEKNonce   []byte `json:"dek_nonce"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// KEK is a key-encryption key held in memory for the process lifetime.
type KEK struct {
	key     []byte
	version int
}

// LoadOrCreateKEK resolves the KEK from POSTRA_KEK (base64, 32 bytes) or a
// key file under dataDir, generating one on first run. The key file is the
// personal-mode fallback; server deployments should inject POSTRA_KEK from
// Vault/OpenBao or an OS keychain wrapper.
func LoadOrCreateKEK(dataDir string) (*KEK, error) {
	if v := os.Getenv("POSTRA_KEK"); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(key) != keySize {
			return nil, errors.New("POSTRA_KEK must be base64 of 32 bytes")
		}
		return &KEK{key: key, version: 1}, nil
	}
	path := filepath.Join(dataDir, "kek.key")
	if b, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(string(b))
		if err != nil || len(key) != keySize {
			return nil, fmt.Errorf("corrupt KEK file %s", path)
		}
		return &KEK{key: key, version: 1}, nil
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return &KEK{key: key, version: 1}, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt seals value under a fresh DEK; aad binds the ciphertext to its
// context (owner, secret type, reference) so envelopes cannot be swapped.
func (k *KEK) Encrypt(value, aad []byte) (*Envelope, error) {
	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	defer zero(dek)

	dataAEAD, err := gcm(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, dataAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := dataAEAD.Seal(nil, nonce, value, aad)

	kekAEAD, err := gcm(k.key)
	if err != nil {
		return nil, err
	}
	dekNonce := make([]byte, kekAEAD.NonceSize())
	if _, err := rand.Read(dekNonce); err != nil {
		return nil, err
	}
	wrapped := kekAEAD.Seal(nil, dekNonce, dek, aad)

	return &Envelope{
		Alg:        "aes-256-gcm",
		KeyVersion: k.version,
		WrappedDEK: wrapped,
		DEKNonce:   dekNonce,
		Nonce:      nonce,
		Ciphertext: ct,
	}, nil
}

// Decrypt opens an envelope. The same aad used at encryption time is required.
func (k *KEK) Decrypt(env *Envelope, aad []byte) ([]byte, error) {
	if env.Alg != "aes-256-gcm" {
		return nil, fmt.Errorf("unsupported algorithm %q", env.Alg)
	}
	kekAEAD, err := gcm(k.key)
	if err != nil {
		return nil, err
	}
	dek, err := kekAEAD.Open(nil, env.DEKNonce, env.WrappedDEK, aad)
	if err != nil {
		return nil, errors.New("unwrap DEK failed (wrong KEK or tampered envelope)")
	}
	defer zero(dek)
	dataAEAD, err := gcm(dek)
	if err != nil {
		return nil, err
	}
	pt, err := dataAEAD.Open(nil, env.Nonce, env.Ciphertext, aad)
	if err != nil {
		return nil, errors.New("decrypt failed (tampered ciphertext or wrong AAD)")
	}
	return pt, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
