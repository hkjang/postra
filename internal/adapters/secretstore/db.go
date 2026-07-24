package secretstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"postra/internal/domain"
	"postra/internal/platform/crypto"
)

// EnvelopeStore is the persistence port the DB-backed secret store depends on.
// Both the SQLite and PostgreSQL adapters satisfy it. Only ciphertext
// (envelope-encrypted values) ever crosses this port.
type EnvelopeStore interface {
	PutSecretEnvelope(ctx context.Context, sec domain.StoredSecret) error
	GetSecretEnvelope(ctx context.Context, ref string) (domain.StoredSecret, error)
	MarkSecretEnvelopeRevoked(ctx context.Context, ref string) error
	ListSecretEnvelopes(ctx context.Context) ([]domain.StoredSecret, error)
}

// DBStore keeps secret envelopes in the shared application database instead of
// a per-node local file. This is what makes a saved mail password survive a
// process restart and be usable from every replica: the ciphertext lives
// beside the CredentialRef that names it, and both are in the same durable,
// shared store. The KEK still guards the plaintext; inject POSTRA_KEK (external
// key management) or let the app persist a shared keyring in the DB.
type DBStore struct {
	store EnvelopeStore
	kek   *crypto.KEK
}

func NewDB(store EnvelopeStore, kek *crypto.KEK) *DBStore {
	return &DBStore{store: store, kek: kek}
}

func (s *DBStore) Put(ctx context.Context, req domain.PutSecretRequest) (domain.SecretRef, error) {
	b := make([]byte, 8)
	rand.Read(b)
	ref := "sec_" + hex.EncodeToString(b)
	env, err := s.kek.Encrypt(req.Value.Reveal(), aad(ref, req.OwnerUserID, req.Type))
	req.Value.Zero()
	if err != nil {
		return "", err
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	if err := s.store.PutSecretEnvelope(ctx, domain.StoredSecret{
		Ref: ref, Owner: req.OwnerUserID, Type: req.Type, Label: req.Label,
		Envelope: string(envJSON), Version: 1,
	}); err != nil {
		return "", err
	}
	return domain.SecretRef(ref), nil
}

func (s *DBStore) Acquire(ctx context.Context, ref domain.SecretRef, purpose domain.SecretPurpose) (*domain.SecretHandle, error) {
	sec, err := s.store.GetSecretEnvelope(ctx, string(ref))
	if errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("secret %s not available in secret store (re-enter password to update)", ref)
	}
	if err != nil {
		return nil, fmt.Errorf("secret store load failed: %w", err)
	}
	if sec.Revoked || sec.Envelope == "" {
		return nil, fmt.Errorf("secret %s not available in secret store (re-enter password to update)", ref)
	}
	var env crypto.Envelope
	if err := json.Unmarshal([]byte(sec.Envelope), &env); err != nil {
		return nil, fmt.Errorf("secret %s envelope corrupt (re-enter password to update): %w", ref, err)
	}
	pt, err := s.kek.Decrypt(&env, aad(string(ref), sec.Owner, sec.Type))
	if err != nil {
		return nil, fmt.Errorf("secret %s decryption failed (KEK/keyring changed; set a stable POSTRA_KEK or re-enter password): %w", ref, err)
	}
	h := domain.NewSecretHandle(pt)
	for i := range pt {
		pt[i] = 0
	}
	return h, nil
}

func (s *DBStore) Rotate(ctx context.Context, ref domain.SecretRef, req domain.RotateSecretRequest) error {
	sec, err := s.store.GetSecretEnvelope(ctx, string(ref))
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if errors.Is(err, domain.ErrNotFound) {
		// Self-heal: the ref exists elsewhere (account/credential_refs) but the
		// value was never stored or was lost. Recreate it under this ref.
		sec = domain.StoredSecret{Ref: string(ref), Owner: "local", Type: domain.SecretMailPassword, Label: "Mail Password", Version: 0}
	}
	env, err := s.kek.Encrypt(req.Value.Reveal(), aad(string(ref), sec.Owner, sec.Type))
	req.Value.Zero()
	if err != nil {
		return err
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return err
	}
	sec.Envelope = string(envJSON)
	sec.Version++
	sec.Revoked = false
	return s.store.PutSecretEnvelope(ctx, sec)
}

func (s *DBStore) Revoke(ctx context.Context, ref domain.SecretRef) error {
	return s.store.MarkSecretEnvelopeRevoked(ctx, string(ref))
}

// RewrapAll re-wraps every stored secret envelope under the KEK's current
// version (§11.3 회전). Returns the number of envelopes rewrapped.
func (s *DBStore) RewrapAll(ctx context.Context) (int, error) {
	list, err := s.store.ListSecretEnvelopes(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sec := range list {
		if sec.Envelope == "" {
			continue
		}
		var env crypto.Envelope
		if err := json.Unmarshal([]byte(sec.Envelope), &env); err != nil {
			return n, fmt.Errorf("rewrap secret %s: %w", sec.Ref, err)
		}
		changed, err := s.kek.Rewrap(&env, aad(sec.Ref, sec.Owner, sec.Type))
		if err != nil {
			return n, fmt.Errorf("rewrap secret %s: %w", sec.Ref, err)
		}
		if !changed {
			continue
		}
		envJSON, err := json.Marshal(&env)
		if err != nil {
			return n, err
		}
		sec.Envelope = string(envJSON)
		if err := s.store.PutSecretEnvelope(ctx, sec); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

var _ domain.SecretStore = (*DBStore)(nil)

// MigrateLocalFileToDB copies any secrets from a legacy local secrets.enc.json
// (under dataDir) into the shared envelope store, skipping refs that already
// exist there. Envelopes are copied verbatim (still ciphertext) — this is safe
// only when the DB keyring was seeded from the same on-disk keyring, which is
// exactly how resolveKEK bootstraps an upgrading single-node deployment.
// Returns the number of secrets migrated. Best-effort: a missing file is not an
// error.
func MigrateLocalFileToDB(ctx context.Context, dataDir string, dst EnvelopeStore) (int, error) {
	local := NewLocal(dataDir, nil)
	m, err := local.load()
	if err != nil {
		return 0, err
	}
	n := 0
	for ref, e := range m {
		if e == nil || e.Revoked || e.Envelope == nil {
			continue
		}
		if existing, err := dst.GetSecretEnvelope(ctx, ref); err == nil && existing.Envelope != "" {
			continue // already present in the shared store
		} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return n, err
		}
		envJSON, err := json.Marshal(e.Envelope)
		if err != nil {
			return n, err
		}
		version := e.Version
		if version < 1 {
			version = 1
		}
		if err := dst.PutSecretEnvelope(ctx, domain.StoredSecret{
			Ref: ref, Owner: e.Owner, Type: e.Type, Label: e.Label,
			Envelope: string(envJSON), Version: version,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
