package domain

// MessageCollab is the shared-mailbox collaboration state for a message:
// who owns it, its work status, and an optional SLA deadline (§협업 공유 메일함).
// It is keyed within the operating user's scope, which fits the common on-prem
// pattern of a team sharing one service account.
type MessageCollab struct {
	MessageID string `json:"message_id"`
	UserID    string `json:"user_id"`
	Assignee  string `json:"assignee,omitempty"`
	Status    string `json:"status"` // new | needs_action | in_progress | waiting | done; legacy aliases remain accepted
	SLADue    int64  `json:"sla_due,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
	// SLANotifiedAt is when the assignee was last mailed about SLADue (unix
	// seconds, 0 = never). Bookkeeping for the deadline notifier, not API
	// surface; it resets whenever the deadline changes.
	SLANotifiedAt int64 `json:"-"`
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
	CollabNew         = "new"
	CollabNeedsAction = "needs_action"
	CollabInProgress  = "in_progress"
	CollabWaiting     = "waiting"
	CollabDone        = "done"
	CollabOpen        = "open"
	CollabPending     = "pending"
	CollabResolved    = "resolved"
)

// CanonicalCollabStatus preserves the meaning of the three legacy states.
// Storage is not destructively rewritten; both forms remain usable by callers.
func CanonicalCollabStatus(status string) (string, bool) {
	switch status {
	case CollabNew, CollabOpen:
		return CollabNew, true
	case CollabNeedsAction:
		return CollabNeedsAction, true
	case CollabInProgress, CollabPending:
		return CollabInProgress, true
	case CollabWaiting:
		return CollabWaiting, true
	case CollabDone, CollabResolved:
		return CollabDone, true
	default:
		return "", false
	}
}

func CollabStatusAliases(status string) []string {
	canonical, _ := CanonicalCollabStatus(status)
	switch canonical {
	case CollabNew:
		return []string{CollabNew, CollabOpen}
	case CollabInProgress:
		return []string{CollabInProgress, CollabPending}
	case CollabDone:
		return []string{CollabDone, CollabResolved}
	default:
		return []string{status}
	}
}
