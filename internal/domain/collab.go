package domain

// MessageCollab is the shared-mailbox collaboration state for a message:
// who owns it, its work status, and an optional SLA deadline (§협업 공유 메일함).
// It is keyed within the operating user's scope, which fits the common on-prem
// pattern of a team sharing one service account.
type MessageCollab struct {
	MessageID string `json:"message_id"`
	UserID    string `json:"user_id"`
	Assignee  string `json:"assignee,omitempty"`
	Status    string `json:"status"` // open | pending | resolved
	SLADue    int64  `json:"sla_due,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// MessageNote is an internal team note attached to a message (not sent).
type MessageNote struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id"`
	UserID    string `json:"user_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
}

// Collaboration work-status vocabulary.
const (
	CollabOpen     = "open"
	CollabPending  = "pending"
	CollabResolved = "resolved"
)
