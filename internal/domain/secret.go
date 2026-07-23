package domain

import (
	"context"
	"errors"
)

// ErrNotFound is the canonical "record does not exist" error returned by any
// Storage adapter, so transports can map it to 404 without importing a
// specific adapter package.
var ErrNotFound = errors.New("not found")

// SecretRef is an opaque reference to a secret held in a SecretStore.
// Only references travel through the application layer, REST DTOs, and MCP
// tool arguments — never secret values (SEC-KEY-001/004).
type SecretRef string

// SecretPurpose is recorded on every acquisition for auditing.
type SecretPurpose string

const (
	PurposePOP3Auth SecretPurpose = "pop3_auth"
	PurposeSMTPAuth SecretPurpose = "smtp_auth"
	PurposeAIKey    SecretPurpose = "ai_api_key"
	PurposeTest     SecretPurpose = "connection_test"
	PurposeOIDC     SecretPurpose = "oidc_client_secret"
)

type SecretType string

const (
	SecretMailPassword SecretType = "mail_password"
	SecretAPIKey       SecretType = "api_key"
)

// SecretHandle carries a decrypted secret for the shortest possible scope.
// It cannot be marshaled or stringified: String/MarshalJSON always redact
// (SEC-KEY-006). Adapters call Reveal immediately before dialing and Zero
// right after (SEC-KEY-005).
type SecretHandle struct {
	value []byte
}

func NewSecretHandle(v []byte) *SecretHandle {
	c := make([]byte, len(v))
	copy(c, v)
	return &SecretHandle{value: c}
}

func (h *SecretHandle) Reveal() []byte {
	if h == nil {
		return nil
	}
	return h.value
}

func (h *SecretHandle) Zero() {
	if h == nil {
		return
	}
	for i := range h.value {
		h.value[i] = 0
	}
	h.value = nil
}

func (h *SecretHandle) String() string { return "[REDACTED]" }

func (h *SecretHandle) MarshalJSON() ([]byte, error) {
	return nil, errors.New("SecretHandle must not be serialized")
}

func (h *SecretHandle) GoString() string { return "[REDACTED]" }

type PutSecretRequest struct {
	OwnerUserID string
	Type        SecretType
	Provider    string // "local", "vault", ...
	Value       *SecretHandle
	Label       string
}

type RotateSecretRequest struct {
	Value *SecretHandle
}

// SecretStore is the provider-independent secret port. Implementations:
// local encrypted file store (personal mode), Vault/OpenBao (server mode).
// There is deliberately no "Get plaintext" API beyond Acquire, and Acquire
// results must stay inside adapters (SEC-KEY-003).
type SecretStore interface {
	Put(ctx context.Context, req PutSecretRequest) (SecretRef, error)
	Acquire(ctx context.Context, ref SecretRef, purpose SecretPurpose) (*SecretHandle, error)
	Rotate(ctx context.Context, ref SecretRef, req RotateSecretRequest) error
	Revoke(ctx context.Context, ref SecretRef) error
}

// CredentialRef is the DB-visible metadata about a secret (never the value).
type CredentialRef struct {
	Ref        SecretRef  `json:"ref"`
	OwnerID    string     `json:"owner_id"`
	Type       SecretType `json:"type"`
	Provider   string     `json:"provider"`
	Label      string     `json:"label"`
	Status     string     `json:"status"` // active | revoked
	Version    int        `json:"version"`
	CreatedAt  int64      `json:"created_at"`
	LastUsedAt int64      `json:"last_used_at,omitempty"`
}
