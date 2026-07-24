package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

// incidentOpt attaches optional correlation context to a recorded incident.
type incidentOpt func(*domain.Incident)

func withIncidentAccount(id string) incidentOpt { return func(i *domain.Incident) { i.AccountID = id } }
func withIncidentJob(id string) incidentOpt      { return func(i *domain.Incident) { i.JobID = id } }

// recordIncident persists a major system error for admin reporting. It is
// strictly best-effort: it never returns an error and never panics into the
// caller (it runs from panic-recovery paths), so a logging failure can't
// cascade. Recurring identical errors collapse into one row (count++), keeping
// the admin view manageable.
func (a *App) recordIncident(severity domain.Severity, component, message, detail string, opts ...incidentOpt) {
	defer func() { _ = recover() }()
	now := time.Now().Unix()
	inc := &domain.Incident{
		ID:        persistence.NewID("inc"),
		Severity:  severity,
		Component: component,
		Message:   truncateRunes(message, 500),
		Detail:    truncateRunes(detail, 8000),
		Count:     1,
		FirstSeen: now,
		LastSeen:  now,
	}
	for _, o := range opts {
		o(inc)
	}
	inc.Fingerprint = incidentFingerprint(component, severity, inc.Message)
	if err := a.Store.RecordIncident(context.Background(), inc); err != nil {
		slog.Warn("failed to record system incident", "component", component, "err", err)
	}
}

// incidentFingerprint groups recurring occurrences of the same fault so they
// dedupe into a single incident row.
func incidentFingerprint(component string, severity domain.Severity, message string) string {
	sum := sha256.Sum256([]byte(component + "|" + string(severity) + "|" + message))
	return hex.EncodeToString(sum[:16])
}

// ---------- admin surface ----------

func (a *App) AdminListIncidents(ctx context.Context, f domain.IncidentFilter) ([]domain.Incident, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return a.Store.ListIncidents(ctx, f)
}

func (a *App) AdminIncidentStats(ctx context.Context) (domain.IncidentStats, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return domain.IncidentStats{}, err
	}
	return a.Store.IncidentStats(ctx)
}

func (a *App) AdminGetIncident(ctx context.Context, id string) (*domain.Incident, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return a.Store.GetIncident(ctx, id)
}

func (a *App) AdminResolveIncident(ctx context.Context, id string) error {
	p, err := requireAdmin(ctx)
	if err != nil {
		return err
	}
	if err := a.Store.ResolveIncident(ctx, id, p.LoginID); err != nil {
		return err
	}
	a.audit(ctx, "incident_resolve", "incident:"+id, "ok", "")
	return nil
}
