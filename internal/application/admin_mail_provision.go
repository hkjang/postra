package application

import (
	"context"
	"errors"
	"strings"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

// AdminMailProvisionInput is the administrator-facing, offline-friendly mail
// provisioning contract. MailPassword is input-only and is never returned or
// persisted in an account record.
type AdminMailProvisionInput struct {
	TargetUser         string `json:"target_user"`
	MailAddress        string `json:"mail_address"`
	IMAPHost           string `json:"imap_host"`
	IMAPPort           int    `json:"imap_port"`
	IMAPSecurity       string `json:"imap_security"`
	SMTPHost           string `json:"smtp_host"`
	SMTPPort           int    `json:"smtp_port"`
	SMTPSecurity       string `json:"smtp_security"`
	SMTPAuth           string `json:"smtp_auth,omitempty"`
	AuthUsername       string `json:"auth_username"`
	MailPassword       string `json:"mail_password"`
	SamePassword       *bool  `json:"same_password,omitempty"`
	SMTPPassword       string `json:"smtp_password,omitempty"`
	AutomaticSync      *bool  `json:"automatic_sync,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

type AdminMailProvisionResult struct {
	User    *domain.User        `json:"user"`
	Account *domain.MailAccount `json:"account"`
}

// AdminProvisionMailAccount links (or pre-creates) an OIDC identity and
// creates an IMAP/SMTP account owned by that user. This works before the
// user's first Keycloak login: the verified email claim completes the subject
// binding on first login.
func (a *App) AdminProvisionMailAccount(ctx context.Context, in AdminMailProvisionInput) (*AdminMailProvisionResult, error) {
	admin, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(in.TargetUser)
	if target == "" {
		return nil, userErrf("target user is required")
	}
	mail := strings.TrimSpace(in.MailAddress)
	if in.MailPassword == "" {
		return nil, userErrf("mail password is required")
	}
	if strings.TrimSpace(in.IMAPHost) == "" || strings.TrimSpace(in.SMTPHost) == "" {
		return nil, userErrf("IMAP and SMTP servers are required")
	}
	lookupMail := mail
	if lookupMail == "" && strings.Contains(target, "@") {
		lookupMail = target
	}
	user, created, err := a.resolveProvisionTarget(ctx, target, lookupMail)
	if err != nil {
		return nil, err
	}
	if mail == "" {
		mail = strings.TrimSpace(user.Email)
	}
	if mail == "" && strings.Contains(target, "@") {
		mail = target
	}
	if mail == "" {
		return nil, userErrf("mail address is required when the target user has no email")
	}
	username := strings.TrimSpace(in.AuthUsername)
	if username == "" {
		username = mail
	}
	ownerCtx := WithPrincipal(ctx, principalFor(user, "admin_provision"))
	label := mail + " 관리자 프로비저닝"
	inHandle := domain.NewSecretHandle([]byte(in.MailPassword))
	inRef, err := a.RegisterSecret(ownerCtx, domain.SecretMailPassword, label, inHandle)
	inHandle.Zero()
	if err != nil {
		if created {
			user.Status = domain.UserDeleted
			_ = a.Store.UpdateUser(ctx, user)
		}
		return nil, err
	}

	same := in.SamePassword == nil || *in.SamePassword
	smtpAuth := strings.TrimSpace(in.SMTPAuth)
	if smtpAuth == "" {
		smtpAuth = "auto"
	}
	if smtpAuth != "auto" && smtpAuth != "none" {
		_ = a.RevokeSecret(ownerCtx, inRef)
		return nil, userErrf("invalid SMTP auth mode")
	}
	smtpRef := inRef
	if smtpAuth != "none" && !same {
		if in.SMTPPassword == "" {
			_ = a.RevokeSecret(ownerCtx, inRef)
			return nil, userErrf("SMTP password is required when shared password is disabled")
		}
		smtpHandle := domain.NewSecretHandle([]byte(in.SMTPPassword))
		smtpRef, err = a.RegisterSecret(ownerCtx, domain.SecretMailPassword, label+" SMTP", smtpHandle)
		smtpHandle.Zero()
		if err != nil {
			_ = a.RevokeSecret(ownerCtx, inRef)
			return nil, err
		}
	}

	acc, err := a.CreateAccount(ownerCtx, CreateAccountInput{
		Name: mail, Email: mail, InboundProtocol: domain.InboundIMAP,
		POP3Host: strings.TrimSpace(in.IMAPHost), POP3Port: in.IMAPPort,
		POP3Security: in.IMAPSecurity, POP3Username: username, POP3SecretRef: string(inRef),
		SMTPHost: strings.TrimSpace(in.SMTPHost), SMTPPort: in.SMTPPort,
		SMTPSecurity: in.SMTPSecurity, SMTPUsername: username, SMTPAuth: smtpAuth,
		SMTPSecretRef: string(smtpRef), InsecureSkipVerify: in.InsecureSkipVerify,
	})
	if err != nil {
		_ = a.RevokeSecret(ownerCtx, inRef)
		if smtpRef != inRef {
			_ = a.RevokeSecret(ownerCtx, smtpRef)
		}
		return nil, err
	}
	// Automatic synchronization is on by default. The existing scheduler
	// synchronizes every active account, including accounts provisioned before
	// first login. An explicit false leaves the account configured but disabled.
	if in.AutomaticSync != nil && !*in.AutomaticSync {
		_ = a.Store.SetAccountStatus(ctx, user.ID, acc.ID, domain.AccountDisabled)
		acc.Status = domain.AccountDisabled
	}
	a.audit(ctx, "admin_mail_provision", "account:"+acc.ID, "ok",
		"admin="+admin.UserID+" user="+user.ID)
	return &AdminMailProvisionResult{User: user, Account: acc}, nil
}

func (a *App) resolveProvisionTarget(ctx context.Context, target, mail string) (*domain.User, bool, error) {
	if u, err := a.Store.GetUser(ctx, target); err == nil {
		return u, false, nil
	}
	if u, _, err := a.Store.GetUserByLogin(ctx, target); err == nil {
		return u, false, nil
	}
	if mail != "" {
		if u, err := a.Store.GetUserByEmail(ctx, mail); err == nil {
			return u, false, nil
		}
	}
	rt, err := a.oidcRuntime(ctx)
	if err != nil {
		return nil, false, err
	}
	if rt.Issuer == "" {
		return nil, false, userErrf("target user was not found and OIDC is not configured")
	}
	loginID := target
	if _, _, err := a.Store.GetUserByLogin(ctx, loginID); err == nil {
		return nil, false, userErrf("target login ID already exists")
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, false, err
	}
	u := &domain.User{
		ID: persistence.NewID("usr"), LoginID: loginID, DisplayName: loginID,
		Email: mail, Role: domain.RoleUser, Status: domain.UserActive,
		AuthProvider: "oidc", OIDCIssuer: rt.Issuer,
	}
	if err := a.Store.CreateUser(ctx, u, ""); err != nil {
		return nil, false, err
	}
	a.audit(ctx, "oidc_user_preprovision", "user:"+u.ID, "ok", rt.Issuer)
	return u, true, nil
}
