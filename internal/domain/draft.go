package domain

import "context"

type DraftKind string

const (
	DraftNew      DraftKind = "new"
	DraftReply    DraftKind = "reply"
	DraftReplyAll DraftKind = "reply_all"
	DraftForward  DraftKind = "forward"
)

type DraftStatus string

const (
	DraftOpen      DraftStatus = "open"
	DraftApproved  DraftStatus = "approved"
	DraftSent      DraftStatus = "sent"
	DraftDiscarded DraftStatus = "discarded"
)

type Draft struct {
	ID               string      `json:"id"`
	UserID           string      `json:"user_id"`
	AccountID        string      `json:"account_id"`
	Kind             DraftKind   `json:"kind"`
	ReplyToMessageID string      `json:"reply_to_message_id,omitempty"`
	Status           DraftStatus `json:"status"`
	CurrentVersion   int         `json:"current_version"`
	CreatedAt        int64       `json:"created_at"`
	UpdatedAt        int64       `json:"updated_at"`
}

// DraftVersion keeps AI-generated and user-edited revisions apart via Author
// (DRAFT-002/003).
type DraftVersion struct {
	DraftID   string    `json:"draft_id"`
	Version   int       `json:"version"`
	Subject   string    `json:"subject"`
	BodyText  string    `json:"body_text"`
	BodyHTML  string    `json:"body_html,omitempty"`
	To        []Address `json:"to"`
	Cc        []Address `json:"cc,omitempty"`
	Bcc       []Address `json:"bcc,omitempty"`
	Author    string    `json:"author"` // "ai" | "user"
	CreatedAt int64     `json:"created_at"`
}

// ApprovalRequest binds an approval to the exact payload being approved
// (account, addresses, subject, body hash, draft version, expiry) so any
// post-approval mutation invalidates the token (§9.2).
type ApprovalRequest struct {
	UserID       string
	ActionType   string // "mail_send" | "server_delete"
	DraftID      string
	DraftVersion int
	PayloadHash  string // SHA-256 over the canonical send payload
	TTLSeconds   int
	Approver     string
}

type ApprovalToken struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	Expires int64  `json:"expires"`
}

type ApprovalService interface {
	Issue(ctx context.Context, req ApprovalRequest) (ApprovalToken, error)
	// VerifyAndConsume atomically validates token+payloadHash and marks the
	// approval used; a second call with the same token fails.
	VerifyAndConsume(ctx context.Context, token string, payloadHash string) error
}

type OutboundStatus string

const (
	OutboundQueued    OutboundStatus = "queued"
	OutboundSent      OutboundStatus = "sent"
	OutboundFailed    OutboundStatus = "failed"
	OutboundUncertain OutboundStatus = "send_uncertain"
)

type OutboundMessage struct {
	ID             string         `json:"id"`
	UserID         string         `json:"user_id"`
	DraftID        string         `json:"draft_id"`
	DraftVersion   int            `json:"draft_version"`
	IdempotencyKey string         `json:"idempotency_key"`
	MessageID      string         `json:"message_id"` // generated Message-ID header
	Status         OutboundStatus `json:"status"`
	SMTPResponse   string         `json:"smtp_response,omitempty"`
	Attempts       int            `json:"attempts"`
	CreatedAt      int64          `json:"created_at"`
	UpdatedAt      int64          `json:"updated_at"`
}
