package domain

import "context"

// MailDeliveryStatus is the lifecycle of one relay notification attempt.
type MailDeliveryStatus string

const (
	MailDeliveryQueued MailDeliveryStatus = "queued"
	MailDeliverySent   MailDeliveryStatus = "sent"
	MailDeliveryFailed MailDeliveryStatus = "failed"
)

// MailDelivery records one event notification handed to the company SMTP
// relay — every attempt, successful or not, so an administrator can answer
// "did it leave the building?". The body is deliberately not stored: the
// subject and recipient are enough, and a stored body would make the log
// itself a leak.
type MailDelivery struct {
	ID        string             `json:"id"`
	Event     string             `json:"event"`
	Recipient string             `json:"recipient"`
	Subject   string             `json:"subject"`
	UserID    string             `json:"user_id,omitempty"`  // account the mail was addressed to
	ActorID   string             `json:"actor_id,omitempty"` // who caused the event ("system" for workers)
	Status    MailDeliveryStatus `json:"status"`
	Attempts  int                `json:"attempts"`
	Error     string             `json:"error,omitempty"`
	CreatedAt int64              `json:"created_at"`
	UpdatedAt int64              `json:"updated_at"`
}

// MailDeliveryFilter narrows the admin delivery listing.
type MailDeliveryFilter struct {
	Status MailDeliveryStatus // "" = any
	Limit  int
}

// MailDeliveryStore persists notification attempts. Implemented by every
// Storage adapter.
type MailDeliveryStore interface {
	RecordMailDelivery(ctx context.Context, d *MailDelivery) error
	// CompleteMailDelivery stores the terminal outcome of a queued delivery.
	CompleteMailDelivery(ctx context.Context, id string, status MailDeliveryStatus, attempts int, errMessage string, updatedAt int64) error
	ListMailDeliveries(ctx context.Context, f MailDeliveryFilter) ([]MailDelivery, error)
	// MailDeliveryCounts returns the number of deliveries per status.
	MailDeliveryCounts(ctx context.Context) (map[MailDeliveryStatus]int64, error)
}
