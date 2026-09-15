package application

import (
	"context"
	"path/filepath"
	"strings"

	"postra/internal/adapters/malware"
	"postra/internal/domain"
)

// ScanAttachment uses the current shared attachment policy for both inbound
// collection and outbound upload. A configured external scanner is retained.
func (a *App) ScanAttachment(ctx context.Context, input domain.ScanInput) domain.ScanVerdict {
	if limit := a.SettingInt("attachments.max_bytes"); limit > 0 && len(input.Data) > limit {
		return domain.ScanVerdict{Status: domain.ScanBlocked, StoreContent: false, Detail: "attachment exceeds administrator size limit"}
	}
	if raw := strings.TrimSpace(a.Setting("attachments.allow_extensions")); raw != "" {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(input.Name), "."))
		allowed := false
		for _, item := range strings.Split(raw, ",") {
			if strings.ToLower(strings.TrimPrefix(strings.TrimSpace(item), ".")) == ext {
				allowed = true
			}
		}
		if !allowed {
			return domain.ScanVerdict{Status: domain.ScanBlocked, StoreContent: false, Detail: "attachment extension is not allowed"}
		}
	}
	// Built-in policy is always enforced, even if an external scanner is
	// installed; AV success must not bypass an administrator extension ban.
	verdict := malware.NewHeuristic(a.EffectiveConfig().Attachments).Scan(ctx, input)
	if verdict.Status != domain.ScanClean {
		return verdict
	}
	if _, builtin := a.Scanner.(*malware.HeuristicScanner); !builtin && a.Scanner != nil {
		return a.Scanner.Scan(ctx, input)
	}
	return verdict
}
