package domain

// ActionCard is an actionable item extracted from a message that a user can
// review, approve, and export to an external system (calendar/Jira/ITSM)
// (§에이전트 액션 실행 카드). Postra never registers anything externally on its
// own — export produces a structured payload the caller/integration applies.
type ActionCard struct {
	ID           string  `json:"id"`
	UserID       string  `json:"user_id"`
	MessageID    string  `json:"message_id"`
	Type         string  `json:"type"` // meeting | todo | approval | inquiry | other
	Title        string  `json:"title"`
	Detail       string  `json:"detail,omitempty"`
	Due          string  `json:"due,omitempty"` // free-text date copied from the mail
	Assignee     string  `json:"assignee,omitempty"`
	Status       string  `json:"status"` // pending | approved | rejected | done | exported
	ExportTarget string  `json:"export_target,omitempty"`
	ExternalRef  string  `json:"external_ref,omitempty"`
	Confidence   float64 `json:"confidence,omitempty"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
}

// Action card status vocabulary.
const (
	ActionCardPending  = "pending"
	ActionCardApproved = "approved"
	ActionCardRejected = "rejected"
	ActionCardDone     = "done"
	ActionCardExported = "exported"
)
