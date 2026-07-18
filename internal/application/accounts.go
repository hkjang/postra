package application

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

type CreateAccountInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`

	// InboundProtocol selects the fetch adapter: "pop3" (default) or "imap".
	// The POP3* fields carry the inbound server coordinates for either.
	InboundProtocol string `json:"inbound_protocol,omitempty"`

	POP3Host     string `json:"pop3_host"`
	POP3Port     int    `json:"pop3_port"`
	POP3Security string `json:"pop3_security"` // tls | starttls | none
	POP3Username string `json:"pop3_username"`
	// POP3SecretRef references a secret registered through the secure
	// registration flow — never a raw password (SEC-KEY-001).
	POP3SecretRef string `json:"pop3_secret_ref"`

	SMTPHost      string `json:"smtp_host"`
	SMTPPort      int    `json:"smtp_port"`
	SMTPSecurity  string `json:"smtp_security"`
	SMTPUsername  string `json:"smtp_username"`
	SMTPAuth      string `json:"smtp_auth"` // auto | none
	SMTPSecretRef string `json:"smtp_secret_ref"`

	InsecureSkipVerify bool `json:"insecure_skip_verify"`
}

func normInboundProtocol(p string) (string, error) {
	switch p {
	case "", domain.InboundPOP3:
		return domain.InboundPOP3, nil
	case domain.InboundIMAP:
		return domain.InboundIMAP, nil
	}
	return "", userErrf("invalid inbound protocol %q (pop3|imap)", p)
}

func normSecurity(s string, def domain.Security) (domain.Security, error) {
	switch domain.Security(s) {
	case "":
		return def, nil
	case domain.SecurityTLS, domain.SecurityStartTLS, domain.SecurityNone:
		return domain.Security(s), nil
	}
	return "", userErrf("invalid security mode %q (tls|starttls|none)", s)
}

func (a *App) CreateAccount(ctx context.Context, in CreateAccountInput) (*domain.MailAccount, error) {
	pop3Sec, err := normSecurity(in.POP3Security, domain.SecurityTLS)
	if err != nil {
		return nil, err
	}
	smtpSec, err := normSecurity(in.SMTPSecurity, domain.SecurityTLS)
	if err != nil {
		return nil, err
	}
	if in.Email == "" {
		return nil, userErrf("email is required")
	}
	if in.SMTPAuth == "" {
		in.SMTPAuth = "auto"
	}
	protocol, err := normInboundProtocol(in.InboundProtocol)
	if err != nil {
		return nil, err
	}
	// Default inbound port depends on protocol: POP3 995/110, IMAP 993/143.
	tlsPort, plainPort := 995, 110
	if protocol == domain.InboundIMAP {
		tlsPort, plainPort = 993, 143
	}
	acc := &domain.MailAccount{
		ID: persistence.NewID("acc"), UserID: DefaultUserID,
		Name: in.Name, Email: in.Email, Status: domain.AccountActive,
		InboundProtocol: protocol,
		POP3Host:        in.POP3Host, POP3Port: portOr(in.POP3Port, pop3Sec, tlsPort, plainPort),
		POP3Security: pop3Sec, POP3Username: in.POP3Username,
		POP3Secret: domain.SecretRef(in.POP3SecretRef),
		SMTPHost:   in.SMTPHost, SMTPPort: portOr(in.SMTPPort, smtpSec, 465, 587),
		SMTPSecurity: smtpSec, SMTPUsername: in.SMTPUsername, SMTPAuth: in.SMTPAuth,
		SMTPSecret:         domain.SecretRef(in.SMTPSecretRef),
		InsecureSkipVerify: in.InsecureSkipVerify,
	}
	for _, h := range []string{acc.POP3Host, acc.SMTPHost} {
		if h == "" {
			continue
		}
		if err := a.validateMailHost(ctx, h); err != nil {
			a.audit(ctx, "account_create", "host:"+h, "denied", err.Error())
			return nil, err
		}
	}
	if err := a.checkInsecureAllowed(ctx, acc); err != nil {
		return nil, err
	}
	if err := a.Store.CreateAccount(ctx, acc); err != nil {
		return nil, err
	}
	a.audit(ctx, "account_create", "account:"+acc.ID, "ok", acc.Email)
	return acc, nil
}

func portOr(p int, sec domain.Security, tlsDefault, plainDefault int) int {
	if p > 0 {
		return p
	}
	if sec == domain.SecurityTLS {
		return tlsDefault
	}
	return plainDefault
}

func (a *App) ListAccounts(ctx context.Context) ([]domain.MailAccount, error) {
	return a.Store.ListAccounts(ctx, DefaultUserID)
}

func (a *App) GetAccount(ctx context.Context, id string) (*domain.MailAccount, error) {
	return a.Store.GetAccount(ctx, DefaultUserID, id)
}

type UpdateAccountInput struct {
	AccountID    string  `json:"account_id"`
	Name         *string `json:"name,omitempty"`
	POP3Host     *string `json:"pop3_host,omitempty"`
	POP3Port     *int    `json:"pop3_port,omitempty"`
	POP3Security *string `json:"pop3_security,omitempty"`
	POP3Username *string `json:"pop3_username,omitempty"`
	SMTPHost     *string `json:"smtp_host,omitempty"`
	SMTPPort     *int    `json:"smtp_port,omitempty"`
	SMTPSecurity *string `json:"smtp_security,omitempty"`
	SMTPUsername *string `json:"smtp_username,omitempty"`
	SMTPAuth     *string `json:"smtp_auth,omitempty"`
}

// UpdateAccount changes non-secret settings only (mail_account_update).
// Secret rotation goes through RotateSecret.
func (a *App) UpdateAccount(ctx context.Context, in UpdateAccountInput) (*domain.MailAccount, error) {
	acc, err := a.Store.GetAccount(ctx, DefaultUserID, in.AccountID)
	if err != nil {
		return nil, err
	}
	setS := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	setS(&acc.Name, in.Name)
	setS(&acc.POP3Host, in.POP3Host)
	setS(&acc.POP3Username, in.POP3Username)
	setS(&acc.SMTPHost, in.SMTPHost)
	setS(&acc.SMTPUsername, in.SMTPUsername)
	setS(&acc.SMTPAuth, in.SMTPAuth)
	if in.POP3Port != nil {
		acc.POP3Port = *in.POP3Port
	}
	if in.SMTPPort != nil {
		acc.SMTPPort = *in.SMTPPort
	}
	if in.POP3Security != nil {
		sec, err := normSecurity(*in.POP3Security, acc.POP3Security)
		if err != nil {
			return nil, err
		}
		acc.POP3Security = sec
	}
	if in.SMTPSecurity != nil {
		sec, err := normSecurity(*in.SMTPSecurity, acc.SMTPSecurity)
		if err != nil {
			return nil, err
		}
		acc.SMTPSecurity = sec
	}
	for _, h := range []string{acc.POP3Host, acc.SMTPHost} {
		if h != "" {
			if err := a.validateMailHost(ctx, h); err != nil {
				return nil, err
			}
		}
	}
	if err := a.checkInsecureAllowed(ctx, acc); err != nil {
		return nil, err
	}
	if err := a.Store.UpdateAccount(ctx, acc); err != nil {
		return nil, err
	}
	a.audit(ctx, "account_update", "account:"+acc.ID, "ok", "")
	return acc, nil
}

func (a *App) DisableAccount(ctx context.Context, id string) error {
	if _, err := a.Store.GetAccount(ctx, DefaultUserID, id); err != nil {
		return err
	}
	if err := a.Store.SetAccountStatus(ctx, DefaultUserID, id, domain.AccountDisabled); err != nil {
		return err
	}
	a.audit(ctx, "account_disable", "account:"+id, "ok", "")
	return nil
}

// TestAccount runs the staged diagnostics of ACC-006/007 for both protocols.
func (a *App) TestAccount(ctx context.Context, id string) ([]domain.ConnDiagnostics, error) {
	acc, err := a.Store.GetAccount(ctx, DefaultUserID, id)
	if err != nil {
		return nil, err
	}
	var out []domain.ConnDiagnostics
	if acc.POP3Host != "" {
		out = append(out, a.testPOP3(ctx, acc))
	}
	if acc.SMTPHost != "" {
		out = append(out, a.testSMTP(ctx, acc))
	}
	a.audit(ctx, "account_test", "account:"+id, "ok", "")
	return out, nil
}

func (a *App) testPOP3(ctx context.Context, acc *domain.MailAccount) domain.ConnDiagnostics {
	diag := domain.ConnDiagnostics{Target: "pop3"}
	step := func(name string, err error) bool {
		st := domain.ConnStep{Step: name, OK: err == nil}
		if err != nil {
			st.Detail = err.Error()
		}
		diag.Steps = append(diag.Steps, st)
		return err == nil
	}
	_, err := net.DefaultResolver.LookupHost(ctx, acc.POP3Host)
	if !step("dns", err) {
		return diag
	}
	var secret *domain.SecretHandle
	if acc.POP3Secret != "" {
		secret, err = a.Secrets.Acquire(ctx, acc.POP3Secret, domain.PurposeTest)
		if !step("secret_acquire", err) {
			return diag
		}
		defer secret.Zero()
	}
	sess, err := a.POP3.Dial(ctx, domain.POP3DialOptions{
		Host: acc.POP3Host, Port: acc.POP3Port, Security: acc.POP3Security,
		Username: acc.POP3Username, Password: secret,
		InsecureSkipVerify: acc.InsecureSkipVerify,
		ConnectTimeoutSec:  a.Cfg.Sync.ConnectTimeoutSec,
		CommandTimeoutSec:  a.Cfg.Sync.CommandTimeoutSec,
	})
	if !step("connect_tls_auth", err) {
		return diag
	}
	defer sess.Close()
	_, err = sess.UIDL(ctx)
	step("uidl", err)
	sess.Quit(ctx)
	diag.OK = true
	return diag
}

func (a *App) testSMTP(ctx context.Context, acc *domain.MailAccount) domain.ConnDiagnostics {
	var secret *domain.SecretHandle
	var err error
	if acc.SMTPSecret != "" && acc.SMTPAuth != "none" {
		secret, err = a.Secrets.Acquire(ctx, acc.SMTPSecret, domain.PurposeTest)
		if err != nil {
			return domain.ConnDiagnostics{Target: "smtp", Steps: []domain.ConnStep{
				{Step: "secret_acquire", OK: false, Detail: err.Error()}}}
		}
		defer secret.Zero()
	}
	diag, err := a.SMTP.TestConnection(ctx, domain.SMTPSendOptions{
		Host: acc.SMTPHost, Port: acc.SMTPPort, Security: acc.SMTPSecurity,
		AuthMethod: acc.SMTPAuth, Username: acc.SMTPUsername, Password: secret,
		InsecureSkipVerify: acc.InsecureSkipVerify,
		ConnectTimeoutSec:  a.Cfg.Sync.ConnectTimeoutSec,
	})
	if err != nil {
		return domain.ConnDiagnostics{Target: "smtp", Steps: []domain.ConnStep{
			{Step: "connect", OK: false, Detail: err.Error()}}}
	}
	return *diag
}

// ---------- secret registration ----------

// RegisterSecret stores a secret value and returns its reference. This is
// only reachable from trusted input paths (CLI TTY prompt, REST over
// loopback/authenticated channel) — the MCP surface exposes only
// secret_registration_begin, which points the client at this flow and never
// carries the value itself (SEC-KEY-001/002, §11.4).
func (a *App) RegisterSecret(ctx context.Context, secretType domain.SecretType, label string, value *domain.SecretHandle) (domain.SecretRef, error) {
	if len(value.Reveal()) == 0 {
		return "", userErrf("secret value is empty")
	}
	ref, err := a.Secrets.Put(ctx, domain.PutSecretRequest{
		OwnerUserID: DefaultUserID, Type: secretType, Provider: "local", Value: value, Label: label,
	})
	if err != nil {
		a.audit(ctx, "secret_register", "", "error", err.Error())
		return "", err
	}
	_ = a.Store.PutCredentialRef(ctx, domain.CredentialRef{
		Ref: ref, OwnerID: DefaultUserID, Type: secretType, Provider: "local",
		Label: label, Status: "active", Version: 1,
	})
	a.audit(ctx, "secret_register", "secret:"+string(ref), "ok", label)
	return ref, nil
}

// RotateSecret replaces a secret value in place; existing accounts keep the
// same reference and pick up the new value on next acquisition without a
// restart (SEC-KEY-010, acceptance #12).
func (a *App) RotateSecret(ctx context.Context, ref domain.SecretRef, value *domain.SecretHandle) error {
	if err := a.Secrets.Rotate(ctx, ref, domain.RotateSecretRequest{Value: value}); err != nil {
		a.audit(ctx, "secret_rotate", "secret:"+string(ref), "error", err.Error())
		return err
	}
	a.audit(ctx, "secret_rotate", "secret:"+string(ref), "ok", "")
	return nil
}

func (a *App) RevokeSecret(ctx context.Context, ref domain.SecretRef) error {
	if err := a.Secrets.Revoke(ctx, ref); err != nil {
		return err
	}
	_ = a.Store.SetCredentialStatus(ctx, ref, "revoked")
	a.audit(ctx, "secret_revoke", "secret:"+string(ref), "ok", "")
	return nil
}

// SecretRegistrationInstructions is returned by the MCP tool
// secret_registration_begin instead of accepting secret material.
func (a *App) SecretRegistrationInstructions() string {
	return fmt.Sprintf(`Secrets are never passed through MCP tool arguments.
Register the credential through one of the secure input paths, then use the returned secret_ref with mail_account_create:
 1. CLI (recommended): postra secret set --type mail_password --label "<label>"  (prompts on TTY, no echo)
 2. REST: POST http://%s/api/secrets {"type":"mail_password","label":"...","value":"..."} over the authenticated local channel
`, a.Cfg.HTTPAddr)
}

var _ = strconv.Itoa // reserved
