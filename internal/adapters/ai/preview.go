package ai

import (
	"context"
	"fmt"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

// NewPreview makes a short-lived provider for a write-only connection test.
// The candidate credential is never persisted or installed in the live app.
func NewPreview(cfg config.AIConfig, store domain.SecretStore, key string) *OpenAICompat {
	if key != "" {
		cfg.APIKeyRef = "connection-preview-only"
		store = &previewSecrets{SecretStore: store, key: key}
	}
	return New(cfg, store)
}

type previewSecrets struct {
	domain.SecretStore
	key string
}

func (p *previewSecrets) Acquire(ctx context.Context, ref domain.SecretRef, purpose domain.SecretPurpose) (*domain.SecretHandle, error) {
	if ref == "connection-preview-only" {
		return domain.NewSecretHandle([]byte(p.key)), nil
	}
	if p.SecretStore == nil {
		return nil, fmt.Errorf("credential store unavailable")
	}
	return p.SecretStore.Acquire(ctx, ref, purpose)
}
