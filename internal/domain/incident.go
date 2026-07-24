package domain

import "context"

// Severity ranks a recorded system incident. Critical means a background
// worker panicked or a core dependency failed (the class of error that used to
// silently crash/restart the server); Error is a failed operation the system
// recovered from; Warning is a degraded-but-handled condition worth reporting.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityError    Severity = "error"
	SeverityWarning  Severity = "warning"
)

// Incident is a persisted record of a notable system error, deduplicated by
// Fingerprint so a recurring fault is one row with a rising Count rather than a
// flood. Admins list, report on, and resolve these.
type Incident struct {
	ID          string   `json:"id"`
	Fingerprint string   `json:"fingerprint"`
	Severity    Severity `json:"severity"`
	Component   string   `json:"component"` // "sync" | "leader-election" | "secret" | "object-store" | ...
	Message     string   `json:"message"`
	Detail      string   `json:"detail,omitempty"` // stack trace / error chain
	Count       int64    `json:"count"`
	FirstSeen   int64    `json:"first_seen"`
	LastSeen    int64    `json:"last_seen"`
	Resolved    bool     `json:"resolved"`
	ResolvedAt  int64    `json:"resolved_at,omitempty"`
	ResolvedBy  string   `json:"resolved_by,omitempty"`
	// Optional correlation context.
	AccountID string `json:"account_id,omitempty"`
	JobID     string `json:"job_id,omitempty"`
}

// IncidentFilter narrows an admin incident listing.
type IncidentFilter struct {
	Severity        Severity // "" = any
	Component       string   // "" = any
	IncludeResolved bool
	Limit           int
}

// IncidentStore persists and queries system incidents. Implemented by every
// Storage adapter.
type IncidentStore interface {
	// RecordIncident upserts by fingerprint among unresolved rows: an existing
	// open incident's Count is incremented and LastSeen bumped; otherwise a new
	// row is inserted. Best-effort at the call site — never fails the caller.
	RecordIncident(ctx context.Context, inc *Incident) error
	ListIncidents(ctx context.Context, f IncidentFilter) ([]Incident, error)
	GetIncident(ctx context.Context, id string) (*Incident, error)
	ResolveIncident(ctx context.Context, id, resolvedBy string) error
	IncidentStats(ctx context.Context) (IncidentStats, error)
}

// IncidentStats is the at-a-glance summary shown on the admin report.
type IncidentStats struct {
	OpenCritical int64 `json:"open_critical"`
	OpenError    int64 `json:"open_error"`
	OpenWarning  int64 `json:"open_warning"`
	OpenTotal    int64 `json:"open_total"`
	Resolved     int64 `json:"resolved"`
}
