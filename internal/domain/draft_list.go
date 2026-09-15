package domain

// DraftSummary deliberately excludes bodies, Bcc and approval credentials.
// The current revision's subject/recipients suffice for the persisted list.
type DraftSummary struct {
	ID             string      `json:"id"`
	AccountID      string      `json:"account_id"`
	Kind           DraftKind   `json:"kind"`
	Status         DraftStatus `json:"status"`
	CurrentVersion int         `json:"current_version"`
	UpdatedAt      int64       `json:"updated_at"`
	Subject        string      `json:"subject"`
	To             []Address   `json:"to"`
	Author         string      `json:"author"`
}
