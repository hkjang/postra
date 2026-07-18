// Package crypto implements envelope encryption (KEK -> DEK -> value)
// with AES-256-GCM, used by the local secret store and object encryption.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

// KEK is a versioned key-encryption keyring held in memory for the process
// lifetime. New data is encrypted under the current version; older versions
// are retained so existing envelopes decrypt until they are rewrapped
// (§11.3 회전, SEC-KEY-010).
type KEK struct {
	keys    map[int][]byte // version -> key
	current int
	path    string // keyring file for persistence (empty when env-injected)
}

type keyringFile struct {
	Current int               `json:"current"`
	Keys    map[string]string `json:"keys"` // version -> base64 key
}

// LoadOrCreateKEK resolves the keyring from POSTRA_KEK (base64, 32 bytes,
// pinned as the only version — key management is external, e.g. Vault) or a
// keyring file under dataDir, migrating a legacy single-key kek.key and
// generating one on first run.
func LoadOrCreateKEK(dataDir string) (*KEK, error) {
	if v := os.Getenv("POSTRA_KEK"); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(key) != keySize {
			return nil, errors.New("POSTRA_KEK must be base64 of 32 bytes")
		}
		return &KEK{keys: map[int][]byte{1: key}, current: 1}, nil
	}

	ringPath := filepath.Join(dataDir, "keyring.json")
	if b, err := os.ReadFile(ringPath); err == nil { // #nosec G304 -- keyring path is under the app-owned data dir
		var kf keyringFile
		if err := json.Unmarshal(b, &kf); err != nil {
			return nil, fmt.Errorf("corrupt keyring %s: %w", ringPath, err)
		}
		k := &KEK{keys: map[int][]byte{}, current: kf.Current, path: ringPath}
		for vs, ks := range kf.Keys {
			var v int
			fmt.Sscanf(vs, "%d", &v)
			key, err := base64.StdEncoding.DecodeString(ks)
			if err != nil || len(key) != keySize {
				return nil, fmt.Errorf("corrupt key v%s in keyring", vs)
			}
			k.keys[v] = key
		}
		if len(k.keys) == 0 || k.keys[k.current] == nil {
			return nil, fmt.Errorf("keyring %s missing current key", ringPath)
		}
		return k, nil
	}

	// Migrate a legacy single-key file, else generate a fresh keyring.
	legacy := filepath.Join(dataDir, "kek.key")
	if b, err := os.ReadFile(legacy); err == nil { // #nosec G304 -- legacy KEK path is under the app-owned data dir
		key, err := base64.StdEncoding.DecodeString(string(b))
		if err != nil || len(key) != keySize {
			return nil, fmt.Errorf("corrupt KEK file %s", legacy)
		}
		k := &KEK{keys: map[int][]byte{1: key}, current: 1, path: ringPath}
		return k, k.save()
	}

	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	k := &KEK{keys: map[int][]byte{1: key}, current: 1, path: ringPath}
	return k, k.save()
}

func (k *KEK) save() error {
	if k.path == "" {
		return nil // env-injected keyring is not persisted
	}
	kf := keyringFile{Current: k.current, Keys: map[string]string{}}
	for v, key := range k.keys {
		kf.Keys[fmt.Sprintf("%d", v)] = base64.StdEncoding.EncodeToString(key)
	}
	b, err := json.MarshalIndent(kf, "", " ")
	if err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.path)
}

// CurrentVersion returns the version new envelopes are encrypted under.
func (k *KEK) CurrentVersion() int { return k.current }

// Rotate generates a new current key version, retaining old versions for
// decryption/rewrap. Returns the new version.
func (k *KEK) Rotate() (int, error) {
	if k.path == "" {
		return 0, errors.New("cannot rotate an externally-injected KEK (POSTRA_KEK); rotate it in the key manager")
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return 0, err
	}
	nv := k.current + 1
	k.keys[nv] = key
	k.current = nv
	return nv, k.save()
}

// RetireVersion removes an old key version after its envelopes are rewrapped.
// The current version cannot be retired.
func (k *KEK) RetireVersion(v int) error {
	if v == k.current {
		return errors.New("cannot retire the current key version")
	}
	delete(k.keys, v)
	return k.save()
}

// Rewrap re-wraps an envelope's DEK under the current key version without
// touching the (DEK-encrypted) ciphertext. Cheap: only the small DEK is
// re-encrypted. Returns true if the envelope changed.
func (k *KEK) Rewrap(env *Envelope, aad []byte) (bool, error) {
	if env.KeyVersion == k.current {
		return false, nil
	}
	oldKey := k.keys[env.KeyVersion]
	if oldKey == nil {
		return false, fmt.Errorf("key version %d not in keyring", env.KeyVersion)
	}
	oldAEAD, err := gcm(oldKey)
	if err != nil {
		return false, err
	}
	dek, err := oldAEAD.Open(nil, env.DEKNonce, env.WrappedDEK, aad)
	if err != nil {
		return false, errors.New("unwrap DEK failed during rewrap")
	}
	defer zero(dek)
	newAEAD, err := gcm(k.keys[k.current])
	if err != nil {
		return false, err
	}
	nonce := make([]byte, newAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return false, err
	}
	env.WrappedDEK = newAEAD.Seal(nil, nonce, dek, aad)
	env.DEKNonce = nonce
	env.KeyVersion = k.current
	return true, nil
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

	kekAEAD, err := gcm(k.keys[k.current])
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
		KeyVersion: k.current,
		WrappedDEK: wrapped,
		DEKNonce:   dekNonce,
		Nonce:      nonce,
		Ciphertext: ct,
	}, nil
}

// Decrypt opens an envelope using the key version it was sealed under.
// The same aad used at encryption time is required.
func (k *KEK) Decrypt(env *Envelope, aad []byte) ([]byte, error) {
	if env.Alg != "aes-256-gcm" {
		return nil, fmt.Errorf("unsupported algorithm %q", env.Alg)
	}
	key := k.keys[env.KeyVersion]
	if key == nil {
		return nil, fmt.Errorf("key version %d not available (retired?)", env.KeyVersion)
	}
	kekAEAD, err := gcm(key)
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
