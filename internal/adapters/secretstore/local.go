// Package secretstore provides SecretStore implementations. LocalStore is
// the personal-mode backend: envelope-encrypted secrets in a single file
// under the data directory. Vault/OpenBao can be added behind the same
// domain.SecretStore port for server deployments.
package secretstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"postra/internal/domain"
	"postra/internal/platform/crypto"
)

type entry struct {
	Envelope *crypto.Envelope  `json:"envelope"`
	Owner    string            `json:"owner"`
	Type     domain.SecretType `json:"type"`
	Label    string            `json:"label"`
	Version  int               `json:"version"`
	Revoked  bool              `json:"revoked"`
}

type LocalStore struct {
	mu   sync.Mutex
	path string
	kek  *crypto.KEK
}

func NewLocal(dataDir string, kek *crypto.KEK) *LocalStore {
	return &LocalStore{path: filepath.Join(dataDir, "secrets.enc.json"), kek: kek}
}

func (s *LocalStore) load() (map[string]*entry, error) {
	m := map[string]*entry{}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("secret store corrupt: %w", err)
	}
	return m, nil
}

func (s *LocalStore) save(m map[string]*entry) error {
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func aad(ref string, owner string, t domain.SecretType) []byte {
	return []byte("postra:" + owner + ":" + string(t) + ":" + ref)
}

func (s *LocalStore) Put(ctx context.Context, req domain.PutSecretRequest) (domain.SecretRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := make([]byte, 8)
	rand.Read(b)
	ref := "sec_" + hex.EncodeToString(b)
	env, err := s.kek.Encrypt(req.Value.Reveal(), aad(ref, req.OwnerUserID, req.Type))
	req.Value.Zero()
	if err != nil {
		return "", err
	}
	m, err := s.load()
	if err != nil {
		return "", err
	}
	m[ref] = &entry{Envelope: env, Owner: req.OwnerUserID, Type: req.Type, Label: req.Label, Version: 1}
	if err := s.save(m); err != nil {
		return "", err
	}
	return domain.SecretRef(ref), nil
}

func (s *LocalStore) Acquire(ctx context.Context, ref domain.SecretRef, purpose domain.SecretPurpose) (*domain.SecretHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	e, ok := m[string(ref)]
	if !ok || e.Revoked {
		return nil, fmt.Errorf("secret %s not available", ref)
	}
	pt, err := s.kek.Decrypt(e.Envelope, aad(string(ref), e.Owner, e.Type))
	if err != nil {
		return nil, err
	}
	h := domain.NewSecretHandle(pt)
	for i := range pt {
		pt[i] = 0
	}
	return h, nil
}

func (s *LocalStore) Rotate(ctx context.Context, ref domain.SecretRef, req domain.RotateSecretRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	e, ok := m[string(ref)]
	if !ok || e.Revoked {
		return fmt.Errorf("secret %s not available", ref)
	}
	env, err := s.kek.Encrypt(req.Value.Reveal(), aad(string(ref), e.Owner, e.Type))
	req.Value.Zero()
	if err != nil {
		return err
	}
	e.Envelope = env
	e.Version++
	return s.save(m)
}

func (s *LocalStore) Revoke(ctx context.Context, ref domain.SecretRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	e, ok := m[string(ref)]
	if !ok {
		return fmt.Errorf("secret %s not found", ref)
	}
	e.Revoked = true
	e.Envelope = nil // drop ciphertext entirely
	return s.save(m)
}

var _ domain.SecretStore = (*LocalStore)(nil)
