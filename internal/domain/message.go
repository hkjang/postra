package domain

import "context"

type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type Message struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"`
	UIDL      string `json:"uidl"`
	// MessageID is the RFC 5322 Message-ID header value.
	MessageID string    `json:"message_id"`
	Subject   string    `json:"subject"`
	From      Address   `json:"from"`
	To        []Address `json:"to,omitempty"`
	Cc        []Address `json:"cc,omitempty"`
	ReplyTo   []Address `json:"reply_to,omitempty"`
	Date      int64     `json:"date"` // unix seconds
	Size      int64     `json:"size"`
	// RawHash is the SHA-256 of the unmodified RFC822 bytes (MIME-013).
	RawHash        string `json:"raw_hash"`
	RawURI         string `json:"raw_uri"` // object store location of the original
	ThreadID       string `json:"thread_id,omitempty"`
	HasAttachments bool   `json:"has_attachments"`
	InReplyTo      string `json:"in_reply_to,omitempty"`
	References     string `json:"references,omitempty"`
	// AuthResults summarizes SPF/DKIM/DMARC from Authentication-Results (MIME-016).
	AuthResults  string   `json:"auth_results,omitempty"`
	ParseError   string   `json:"parse_error,omitempty"` // partial-parse marker (MIME-004)
	CreatedAt    int64    `json:"created_at"`
	IsArchived   bool     `json:"is_archived,omitempty"`
	IsImportant  bool     `json:"is_important,omitempty"`
	SnoozedUntil int64    `json:"snoozed_until,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	// LegalHold, when set, blocks local deletion of the message for compliance
	// / e-discovery retention (§규정 준수 보존·법적 보류).
	LegalHold bool `json:"legal_hold,omitempty"`
}

// EmailAuthResult is the structured interpretation of a message's
// Authentication-Results header (SPF/DKIM/DMARC/ARC) plus an alignment check
// and an overall sender-domain risk score (§보안 이메일 인증 검증).
type EmailAuthResult struct {
	SPF        string   `json:"spf"`   // pass|fail|softfail|neutral|none|temperror|permerror|unknown
	DKIM       string   `json:"dkim"`  // pass|fail|none|...
	DMARC      string   `json:"dmarc"` // pass|fail|none|...
	ARC        string   `json:"arc,omitempty"`
	SPFDomain  string   `json:"spf_domain,omitempty"`
	DKIMDomain string   `json:"dkim_domain,omitempty"`
	FromDomain string   `json:"from_domain,omitempty"`
	Aligned    bool     `json:"aligned"`
	RiskScore  int      `json:"risk_score"` // 0-100
	RiskLevel  string   `json:"risk_level"` // low|medium|high
	Reasons    []string `json:"reasons,omitempty"`
	Raw        string   `json:"raw,omitempty"`
}

type MessageBody struct {
	MessageID string `json:"message_id"`
	TextBody  string `json:"text_body"`
	// HTMLSanitized is stripped of scripts, event handlers, and external
	// resources (MIME-008/009); the untouched original stays in RawURI.
	HTMLSanitized string `json:"html_sanitized,omitempty"`
	Charset       string `json:"charset,omitempty"`
}

type Attachment struct {
	ID         string `json:"id"`
	MessageID  string `json:"message_id"`
	Name       string `json:"name"` // sanitized (MIME-010)
	MIMEType   string `json:"mime_type"`
	Size       int64  `json:"size"`
	Hash       string `json:"hash"`
	StorageURI string `json:"storage_uri"` // empty when content was not retained (blocked)
	Inline     bool   `json:"inline"`
	// ScanStatus is the malware/policy scan result (MIME-015).
	ScanStatus ScanStatus `json:"scan_status"`
	ScanDetail string     `json:"scan_detail,omitempty"`
}

// ScanStatus is the disposition of an attachment after policy + archive scan.
type ScanStatus string

const (
	ScanClean       ScanStatus = "clean"       // safe to serve
	ScanQuarantined ScanStatus = "quarantined" // stored but flagged (e.g. executable); download requires ack
	ScanBlocked     ScanStatus = "blocked"     // content not retained (dangerous extension / zip bomb)
	ScanSuspect     ScanStatus = "suspect"     // stored but flagged suspicious
	ScanPending     ScanStatus = "pending"     // awaiting an external scanner
)

// ScanInput is what the AttachmentScanner inspects.
type ScanInput struct {
	Name     string
	MIMEType string
	Data     []byte
}

// ScanVerdict is the scanner's decision.
type ScanVerdict struct {
	Status ScanStatus
	Detail string
	// StoreContent is false when the blob must not be retained (blocked).
	StoreContent bool
}

// AttachmentScanner classifies attachments by policy and inspects archives
// (MIME-011/012/015). The default heuristic implementation is provider-
// independent; a real AV (e.g. ClamAV) can be plugged behind this port.
type AttachmentScanner interface {
	Scan(ctx context.Context, in ScanInput) ScanVerdict
}

type Thread struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	AccountID     string `json:"account_id"`
	SubjectKey    string `json:"subject_key"`
	LastMessageAt int64  `json:"last_message_at"`
	MessageCount  int    `json:"message_count"`
}

type SearchQuery struct {
	UserID        string `json:"-"`
	AccountID     string `json:"account_id,omitempty"`
	Text          string `json:"text,omitempty"`
	From          string `json:"from,omitempty"`
	To            string `json:"to,omitempty"`
	Subject       string `json:"subject,omitempty"`
	Since         int64  `json:"since,omitempty"` // unix seconds
	Until         int64  `json:"until,omitempty"`
	HasAttachment *bool  `json:"has_attachment,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
	Folder        string `json:"folder,omitempty"` // "inbox", "important", "archive", "snoozed"
	Label         string `json:"label,omitempty"`
	IsImportant   *bool  `json:"is_important,omitempty"`
	IsArchived    *bool  `json:"is_archived,omitempty"`
}

type SearchResult struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"next_cursor,omitempty"`
}
