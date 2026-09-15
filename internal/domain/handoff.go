package domain

// HandoffClaim is the stored half of a single-use ticket that lets another
// in-house service collect one message as a document. Only the SHA-256
// digest of the token is kept: a read of the table yields nothing anybody
// could present. The row is deleted when the document is collected — that is
// what single use means — and expired rows are swept when the next claim is
// issued. The document itself is rendered again at collection, so no
// plaintext copy of a mail body sits outside the encrypted body column.
type HandoffClaim struct {
	Digest      string `json:"-"`
	UserID      string `json:"user_id"`
	MessageID   string `json:"message_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	ExpiresAt   int64  `json:"expires_at"` // unix seconds
	CreatedAt   int64  `json:"created_at"`
}
