package domain

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
	AuthResults string `json:"auth_results,omitempty"`
	ParseError  string `json:"parse_error,omitempty"` // partial-parse marker (MIME-004)
	CreatedAt   int64  `json:"created_at"`
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
	StorageURI string `json:"storage_uri"`
	Inline     bool   `json:"inline"`
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
}

type SearchResult struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"next_cursor,omitempty"`
}
