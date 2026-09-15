package application

import (
	"context"
	"net/http"
	"strings"
	"time"

	"postra/internal/domain"
)

func settingsConflict() error {
	return &domain.PublicError{Code: "conflict", Message: "설정이 다른 세션에서 변경되었습니다. 새로고침 후 다시 저장하세요.", Status: http.StatusConflict}
}

func (a *App) commitAdminSettings(ctx context.Context, expected, updates map[string]string) error {
	store, ok := a.Store.(domain.AdminSettingsCompareAndSwapper)
	if !ok {
		// The process-local mutex cannot emulate cross-process atomicity. Custom
		// adapters must explicitly provide the capability instead of silently
		// falling back to an unsafe read/hash/write sequence.
		return &domain.PublicError{Code: "unavailable", Message: "이 저장소는 원자적 관리자 설정 저장을 지원하지 않습니다.", Status: http.StatusServiceUnavailable}
	}
	committed, err := store.CompareAndSwapAdminSettings(ctx, expected, updates)
	if err != nil {
		return err
	}
	if !committed {
		return settingsConflict()
	}
	return nil
}

func (a *App) revokeUnreferencedSettingSecrets(refs []domain.SecretRef) {
	if len(refs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A lost COMMIT response may be an error even though the database persisted
	// the new reference. Do not revoke a now-active key, or guess when the store
	// is unreachable. An unreferenced orphan is safer than disabling a live key.
	stored, err := a.Store.GetSettings(ctx)
	if err != nil {
		return
	}
	// Historical installations may reuse a reference for an AI gateway, OIDC,
	// a task-route JSON setting, or an account credential. Only metadata is read;
	// a failed lookup conservatively keeps the encrypted secret.
	users, err := a.Store.ListUsers(ctx)
	if err != nil {
		return
	}
	accountRefs := map[domain.SecretRef]bool{}
	for _, user := range users {
		accounts, err := a.Store.ListAccounts(ctx, user.ID)
		if err != nil {
			return
		}
		for _, account := range accounts {
			accountRefs[account.POP3Secret] = true
			accountRefs[account.SMTPSecret] = true
		}
	}
	for _, ref := range refs {
		if ref == "" || accountRefs[ref] {
			continue
		}
		referenced := false
		for _, value := range stored {
			if strings.Contains(value, string(ref)) {
				referenced = true
				break
			}
		}
		if referenced {
			continue
		}
		_ = a.RevokeSecret(ctx, ref)
	}
}
