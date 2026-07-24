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

// ---------- process lifecycle (crash evidence) ----------

// bootStateKeyPrefix namespaces the per-host boot sentinel in system_settings.
// "running" means the host booted and has not shut down cleanly yet; "clean"
// is written on graceful shutdown. A host that boots while its own sentinel
// still says "running" was killed without warning (OOMKill, liveness-probe
// kill, SIGKILL) — exactly the class of death that leaves no log and no
// incident, which is why it must be detected after the fact.
const bootStateKeyPrefix = "internal.boot_state."

// IngestCrashReport records a fatal-error dump captured by debug.SetCrashOutput
// during the previous run. Unlike guarded panics, fatal runtime errors
// (concurrent map writes, unrecovered panics in third-party goroutines, ...)
// kill the process before any incident can be written — the dump file is the
// only evidence, so it is surfaced on the next boot.
func (a *App) IngestCrashReport(detail string) {
	a.recordIncident(domain.SeverityCritical, "process",
		"서버가 런타임 치명 오류(fatal error)로 종료됨 — 이전 실행의 크래시 덤프", detail)
}

// NoteBootAndDetectUncleanShutdown marks this host as running and, if the
// previous run on the same host never reached a clean shutdown, records a
// critical incident. hadCrashReport suppresses the generic incident when the
// crash dump already explains the death.
func (a *App) NoteBootAndDetectUncleanShutdown(ctx context.Context, hostname string, hadCrashReport bool) {
	key := bootStateKeyPrefix + hostname
	if settings, err := a.Store.GetSettings(ctx); err == nil && settings[key] == "running" && !hadCrashReport {
		a.recordIncident(domain.SeverityCritical, "process",
			"비정상 종료 감지 — 이전 실행이 정상 종료 없이 중단됨 (OOMKill/liveness kill 가능성)",
			"host="+hostname+"; 크래시 덤프가 없으므로 Go 런타임 오류가 아니라 외부 강제 종료로 추정: "+
				"K8s라면 kubectl describe pod에서 lastState.terminated.reason(OOMKilled 여부)과 "+
				"livenessProbe 대상(/api/livez 권장, /api/healthz는 DB 의존이라 부하 시 kill 유발)을 확인하세요.")
	}
	_ = a.Store.UpsertSettings(ctx, map[string]string{key: "running"})
}

// MarkCleanShutdown records that this host exited gracefully, so the next boot
// does not report an unclean shutdown.
func (a *App) MarkCleanShutdown(ctx context.Context, hostname string) {
	_ = a.Store.UpsertSettings(ctx, map[string]string{bootStateKeyPrefix + hostname: "clean"})
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
