package domain

// MailSignature belongs to exactly one Postra user. AccountID optionally limits
// it to one of that user's mail accounts; no transport may choose another owner.
type MailSignature struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	Name        string `json:"name"`
	AccountID   string `json:"account_id,omitempty"`
	BodyHTML    string `json:"body_html,omitempty"`
	BodyText    string `json:"body_text"`
	DisplayName string `json:"display_name,omitempty"`
	Title       string `json:"title,omitempty"`
	Department  string `json:"department,omitempty"`
	Company     string `json:"company,omitempty"`
	Phone       string `json:"phone,omitempty"`
	Email       string `json:"email,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}
