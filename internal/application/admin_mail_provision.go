package application

import (
	"context"
	"fmt"
	"net/mail"
	"strconv"
	"strings"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

// AdminMailProvisionInput is the administrator-facing, offline-friendly mail
// provisioning contract. MailPassword is input-only and is never returned or
// persisted in an account record.
type AdminMailProvisionInput struct {
	Email              string `json:"email"`
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
	ApplyToAllUsers    bool   `json:"apply_to_all_users,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

type AdminMailProvisionResult struct {
	User    *domain.User        `json:"user,omitempty"`
	Account *domain.MailAccount `json:"account,omitempty"`
}

// AdminProvisionMailAccount stores an organization-wide OIDC mail policy when
// Email is omitted. With an email it also pre-creates that OIDC identity and
// its owned IMAP/SMTP account. A verified Keycloak email completes the binding
// and applies the policy automatically on first login.
func (a *App) AdminProvisionMailAccount(ctx context.Context, in AdminMailProvisionInput) (*AdminMailProvisionResult, error) {
	admin, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	if in.MailPassword == "" {
		return nil, userErrf("mail password is required")
	}
	if strings.TrimSpace(in.IMAPHost) == "" || strings.TrimSpace(in.SMTPHost) == "" {
		return nil, userErrf("IMAP and SMTP servers are required")
	}
	smtpAuth := strings.TrimSpace(in.SMTPAuth)
	if smtpAuth == "" {
		smtpAuth = "auto"
	}
	if smtpAuth != "auto" && smtpAuth != "none" {
		return nil, userErrf("invalid SMTP auth mode")
	}
	in.SMTPAuth = smtpAuth
	if err := a.validateOIDCMailProvisionPolicy(ctx, in); err != nil {
		return nil, err
	}
	mailAddress := strings.TrimSpace(in.Email)
	var user *domain.User
	created := false
	if mailAddress != "" {
		mailAddress, err = normalizedProvisionEmail(mailAddress)
		if err != nil {
			return nil, err
		}
		user, created, err = a.resolveProvisionTarget(ctx, mailAddress)
		if err != nil {
			return nil, err
		}
	}
	username := strings.TrimSpace(in.AuthUsername)
	if username == "" && mailAddress != "" {
		username = mailAddress
	}
	ownerCtx := ctx
	label := "SSO 조직 공통 메일 정책"
	if user != nil {
		ownerCtx = WithPrincipal(ctx, principalFor(user, "admin_provision"))
		label = mailAddress + " 관리자 프로비저닝"
	}
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

	var acc *domain.MailAccount
	if user != nil {
		acc, err = a.CreateAccount(ownerCtx, CreateAccountInput{
			Name: mailAddress, Email: mailAddress, InboundProtocol: domain.InboundIMAP,
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
		if in.AutomaticSync != nil && !*in.AutomaticSync {
			_ = a.Store.SetAccountStatus(ctx, user.ID, acc.ID, domain.AccountDisabled)
			acc.Status = domain.AccountDisabled
		}
	}
	// Omitting an email means "save an organization policy"; applying it to all
	// future verified SSO users is therefore implicit.
	if in.ApplyToAllUsers || user == nil {
		if err := a.saveOIDCMailProvisionPolicy(ctx, in, inRef, smtpRef); err != nil {
			return nil, err
		}
	}
	resource, detail := "oidc-mail-policy", "admin="+admin.UserID
	if user != nil {
		resource, detail = "account:"+acc.ID, detail+" user="+user.ID
	}
	a.audit(ctx, "admin_mail_provision", resource, "ok", detail)
	return &AdminMailProvisionResult{User: user, Account: acc}, nil
}

func (a *App) validateOIDCMailProvisionPolicy(ctx context.Context, in AdminMailProvisionInput) error {
	imapSecurity, err := normSecurity(in.IMAPSecurity, domain.SecurityTLS)
	if err != nil {
		return err
	}
	smtpSecurity, err := normSecurity(in.SMTPSecurity, domain.SecurityTLS)
	if err != nil {
		return err
	}
	for _, host := range []string{in.IMAPHost, in.SMTPHost} {
		if err := a.validateMailHost(ctx, host); err != nil {
			return err
		}
	}
	return a.checkInsecureAllowed(ctx, &domain.MailAccount{
		ID: "oidc-mail-policy", InboundProtocol: domain.InboundIMAP,
		POP3Host: strings.TrimSpace(in.IMAPHost), POP3Port: portOr(in.IMAPPort, imapSecurity, 993, 143),
		POP3Security: imapSecurity, SMTPHost: strings.TrimSpace(in.SMTPHost),
		SMTPPort:     portOr(in.SMTPPort, smtpSecurity, 465, 587),
		SMTPSecurity: smtpSecurity, SMTPAuth: in.SMTPAuth,
		InsecureSkipVerify: in.InsecureSkipVerify,
	})
}

func normalizedProvisionEmail(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	addr, err := mail.ParseAddress(value)
	if err != nil || !strings.EqualFold(addr.Address, value) {
		return "", userErrf("a valid email address is required")
	}
	return value, nil
}

func (a *App) resolveProvisionTarget(ctx context.Context, email string) (*domain.User, bool, error) {
	if u, err := a.Store.GetUserByEmail(ctx, email); err == nil {
		return u, false, nil
	}
	// Upgrade old soft-deleted rows created before identity tombstoning was
	// introduced, so their login/email/OIDC uniqueness no longer blocks a
	// clean test or re-provision of the same Keycloak email.
	if users, err := a.Store.ListUsers(ctx); err == nil {
		for i := range users {
			u := &users[i]
			if u.Status == domain.UserDeleted &&
				(strings.EqualFold(u.Email, email) || strings.EqualFold(u.LoginID, email)) {
				tombstoneUserIdentity(u)
				if err := a.Store.UpdateUser(ctx, u); err != nil {
					return nil, false, err
				}
			}
		}
	}
	rt, err := a.oidcRuntime(ctx)
	if err != nil {
		return nil, false, err
	}
	if rt.Issuer == "" {
		return nil, false, userErrf("target user was not found and OIDC is not configured")
	}
	u := &domain.User{
		ID: persistence.NewID("usr"), LoginID: email, DisplayName: email,
		Email: email, Role: domain.RoleUser, Status: domain.UserActive,
		AuthProvider: "oidc", OIDCIssuer: rt.Issuer,
	}
	if err := a.Store.CreateUser(ctx, u, ""); err != nil {
		return nil, false, err
	}
	a.audit(ctx, "oidc_user_preprovision", "user:"+u.ID, "ok", rt.Issuer)
	return u, true, nil
}

const (
	settingOIDCMailEnabled      = "oidc.mail.enabled"
	settingOIDCMailIMAPHost     = "oidc.mail.imap_host"
	settingOIDCMailIMAPPort     = "oidc.mail.imap_port"
	settingOIDCMailIMAPSecurity = "oidc.mail.imap_security"
	settingOIDCMailSMTPHost     = "oidc.mail.smtp_host"
	settingOIDCMailSMTPPort     = "oidc.mail.smtp_port"
	settingOIDCMailSMTPSecurity = "oidc.mail.smtp_security"
	settingOIDCMailSMTPAuth     = "oidc.mail.smtp_auth"
	settingOIDCMailInboundRef   = "oidc.mail.inbound_secret_ref"
	settingOIDCMailSMTPRef      = "oidc.mail.smtp_secret_ref"
	settingOIDCMailAutoSync     = "oidc.mail.automatic_sync"
	settingOIDCMailSkipVerify   = "oidc.mail.insecure_skip_verify"
)

func (a *App) saveOIDCMailProvisionPolicy(ctx context.Context, in AdminMailProvisionInput, inboundRef, smtpRef domain.SecretRef) error {
	autoSync := in.AutomaticSync == nil || *in.AutomaticSync
	return a.Store.UpsertSettings(ctx, map[string]string{
		settingOIDCMailEnabled: "true", SettingOIDCAutoProvision: "true",
		settingOIDCMailIMAPHost: strings.TrimSpace(in.IMAPHost),
		settingOIDCMailIMAPPort: strconv.Itoa(in.IMAPPort), settingOIDCMailIMAPSecurity: in.IMAPSecurity,
		settingOIDCMailSMTPHost: strings.TrimSpace(in.SMTPHost), settingOIDCMailSMTPPort: strconv.Itoa(in.SMTPPort),
		settingOIDCMailSMTPSecurity: in.SMTPSecurity, settingOIDCMailSMTPAuth: in.SMTPAuth,
		settingOIDCMailInboundRef: string(inboundRef), settingOIDCMailSMTPRef: string(smtpRef),
		settingOIDCMailAutoSync:   strconv.FormatBool(autoSync),
		settingOIDCMailSkipVerify: strconv.FormatBool(in.InsecureSkipVerify),
	})
}

// autoProvisionOIDCMail applies the administrator's shared offline-mail
// policy after a signed Keycloak login. Failure never blocks SSO login.
func (a *App) autoProvisionOIDCMail(ctx context.Context, u *domain.User) error {
	values, err := a.Store.GetSettings(ctx)
	if err != nil || values[settingOIDCMailEnabled] != "true" {
		return err
	}
	email, err := normalizedProvisionEmail(u.Email)
	if err != nil {
		return err
	}
	ownerCtx := WithPrincipal(ctx, principalFor(u, "oidc_auto_provision"))
	accounts, err := a.ListAccounts(ownerCtx)
	if err != nil {
		return err
	}
	for i := range accounts {
		if strings.EqualFold(accounts[i].Email, email) && accounts[i].Status != domain.AccountDeleted {
			a.startInitialOIDCMailSync(ownerCtx, &accounts[i])
			return nil
		}
	}
	imapPort, _ := strconv.Atoi(values[settingOIDCMailIMAPPort])
	smtpPort, _ := strconv.Atoi(values[settingOIDCMailSMTPPort])
	acc, err := a.CreateAccount(ownerCtx, CreateAccountInput{
		Name: email, Email: email, InboundProtocol: domain.InboundIMAP,
		POP3Host: values[settingOIDCMailIMAPHost], POP3Port: imapPort,
		POP3Security: values[settingOIDCMailIMAPSecurity], POP3Username: email,
		POP3SecretRef: values[settingOIDCMailInboundRef],
		SMTPHost:      values[settingOIDCMailSMTPHost], SMTPPort: smtpPort,
		SMTPSecurity: values[settingOIDCMailSMTPSecurity], SMTPUsername: email,
		SMTPAuth: values[settingOIDCMailSMTPAuth], SMTPSecretRef: values[settingOIDCMailSMTPRef],
		InsecureSkipVerify: values[settingOIDCMailSkipVerify] == "true",
	})
	if err != nil {
		return err
	}
	if values[settingOIDCMailAutoSync] == "false" {
		_ = a.Store.SetAccountStatus(ctx, u.ID, acc.ID, domain.AccountDisabled)
		acc.Status = domain.AccountDisabled
	}
	a.startInitialOIDCMailSync(ownerCtx, acc)
	a.audit(ownerCtx, "oidc_mail_auto_provision", "account:"+acc.ID, "ok", fmt.Sprintf("email=%s", email))
	return nil
}

func (a *App) startInitialOIDCMailSync(ctx context.Context, acc *domain.MailAccount) {
	if acc == nil || acc.Status != domain.AccountActive {
		return
	}
	if _, err := a.StartSync(ctx, acc.ID, SyncOptions{}); err != nil {
		// A concurrent scheduler/IDLE sync is benign; any real failure is also
		// visible as an incident without turning a successful SSO login into an
		// error page or removing the newly created account.
		a.audit(ctx, "oidc_initial_sync", "account:"+acc.ID, "error", err.Error())
		return
	}
	a.audit(ctx, "oidc_initial_sync", "account:"+acc.ID, "ok", "")
}

func (a *App) tryAutoProvisionOIDCMail(ctx context.Context, u *domain.User) {
	if err := a.autoProvisionOIDCMail(ctx, u); err != nil {
		a.audit(WithPrincipal(ctx, principalFor(u, "oidc")), "oidc_mail_auto_provision", "user:"+u.ID, "error", err.Error())
		a.recordIncident(domain.SeverityWarning, "oidc-mail", "SSO 메일 자동 프로비저닝 실패", err.Error())
	}
}
