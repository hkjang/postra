package domain

import (
	"context"
	"io"
)

// Security is the transport security mode of a mail connection.
// "none" (plaintext) exists for offline / air-gapped internal networks and
// is only honored when the server policy AllowInsecureMail is enabled.
type Security string

const (
	SecurityTLS      Security = "tls"      // implicit TLS (POP3S 995 / SMTPS 465)
	SecurityStartTLS Security = "starttls" // plaintext connect, upgrade (STLS/STARTTLS)
	SecurityNone     Security = "none"     // plaintext end-to-end (offline networks only)
)

type AccountStatus string

const (
	AccountActive          AccountStatus = "active"
	AccountDisabled        AccountStatus = "disabled"
	AccountCredentialError AccountStatus = "credential_error"
)

type MailAccount struct {
	ID     string        `json:"id"`
	UserID string        `json:"user_id"`
	Name   string        `json:"name"`
	Email  string        `json:"email"`
	Status AccountStatus `json:"status"`

	POP3Host     string    `json:"pop3_host"`
	POP3Port     int       `json:"pop3_port"`
	POP3Security Security  `json:"pop3_security"`
	POP3Username string    `json:"pop3_username"`
	POP3Secret   SecretRef `json:"pop3_secret_ref,omitempty"`

	SMTPHost     string   `json:"smtp_host"`
	SMTPPort     int      `json:"smtp_port"`
	SMTPSecurity Security `json:"smtp_security"`
	SMTPUsername string   `json:"smtp_username"`
	// SMTPAuth: "auto" negotiates, "none" skips AUTH entirely
	// (open relays on isolated networks).
	SMTPAuth   string    `json:"smtp_auth"`
	SMTPSecret SecretRef `json:"smtp_secret_ref,omitempty"`

	// InsecureSkipVerify disables TLS certificate verification. Forbidden by
	// default; requires AllowInsecureMail policy and is always audited.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// ConnStep is one stage of a connection diagnostic (ACC-006/007).
type ConnStep struct {
	Step   string `json:"step"` // dns | tcp | tls | auth | uidl | smtp_ehlo | smtp_auth
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type ConnDiagnostics struct {
	Target string     `json:"target"` // "pop3" | "smtp"
	Steps  []ConnStep `json:"steps"`
	OK     bool       `json:"ok"`
}

// RemoteMessage is a message listed in the POP3 maildrop.
type RemoteMessage struct {
	Number int    // POP3 message number (session-scoped)
	UIDL   string // unique-id (persistent per RFC 1939) — may be empty
	Size   int64
}

// POP3Session is an authenticated POP3 connection. Sessions are produced by
// a POP3Dialer; the secret handle is consumed inside the adapter only.
type POP3Session interface {
	List(ctx context.Context) ([]RemoteMessage, error)
	UIDL(ctx context.Context) ([]RemoteMessage, error)
	Retrieve(ctx context.Context, number int) (io.ReadCloser, error)
	Top(ctx context.Context, number int, lines int) (io.ReadCloser, error)
	Delete(ctx context.Context, number int) error
	Quit(ctx context.Context) error
	Close() error
}

type POP3DialOptions struct {
	Host               string
	Port               int
	Security           Security
	Username           string
	Password           *SecretHandle // nil = no authentication
	InsecureSkipVerify bool
	ConnectTimeoutSec  int
	CommandTimeoutSec  int
}

type POP3Dialer interface {
	Dial(ctx context.Context, opts POP3DialOptions) (POP3Session, error)
}

type Envelope struct {
	From string
	To   []string
}

type SendReceipt struct {
	ServerResponse string
	// Uncertain is set when the DATA payload was handed to the server but the
	// final response was lost — the message may or may not have been accepted
	// (SMTP-008). Callers must not auto-retry an uncertain send.
	Uncertain bool
}

type SMTPSendOptions struct {
	Host               string
	Port               int
	Security           Security
	AuthMethod         string // "auto" | "none"
	Username           string
	Password           *SecretHandle
	InsecureSkipVerify bool
	ConnectTimeoutSec  int
}

// SMTPClient wraps a maintained SMTP implementation behind a stable port
// (net/smtp is frozen upstream).
type SMTPClient interface {
	TestConnection(ctx context.Context, opts SMTPSendOptions) (*ConnDiagnostics, error)
	Send(ctx context.Context, opts SMTPSendOptions, env Envelope, message io.Reader) (SendReceipt, error)
}
