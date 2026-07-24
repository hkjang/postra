package application

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

type OIDCFlow struct {
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"code_verifier"`
	ExpiresAt    int64  `json:"expires_at"`
}

type oidcRuntime struct {
	Issuer, ClientID, ClientSecret, SecretRef, RedirectURL, AdminGroup string
	AutoProvision                                                      bool
}

func (a *App) oidcRuntime(ctx context.Context) (oidcRuntime, error) {
	values, err := a.SystemSettings(ctx)
	if err != nil {
		return oidcRuntime{}, err
	}
	// Trim stored values: a stray trailing space/newline (easy to paste into a
	// settings form) in issuer or redirect_uri makes Keycloak reject the
	// request with "Invalid parameter: redirect_uri" / discovery failures.
	rt := oidcRuntime{
		Issuer: strings.TrimSpace(values[SettingOIDCIssuer]), ClientID: strings.TrimSpace(values[SettingOIDCClientID]),
		SecretRef: strings.TrimSpace(values[SettingOIDCSecretRef]), RedirectURL: strings.TrimSpace(values[SettingOIDCRedirectURL]),
		AdminGroup:    strings.TrimSpace(values[SettingOIDCAdminGroup]),
		AutoProvision: boolSetting(values, SettingOIDCAutoProvision, a.Cfg.Auth.OIDCAutoProvision),
		ClientSecret:  a.Cfg.Auth.OIDCClientSecret,
	}
	if rt.ClientSecret == "" && rt.SecretRef != "" {
		handle, err := a.Secrets.Acquire(ctx, domain.SecretRef(rt.SecretRef), domain.PurposeOIDC)
		if err != nil {
			return rt, err
		}
		rt.ClientSecret = string(handle.Reveal())
		handle.Zero()
	}
	return rt, nil
}

func (a *App) OIDCConfigured(ctx context.Context) bool {
	rt, err := a.oidcRuntime(ctx)
	return err == nil && rt.Issuer != "" && rt.ClientID != "" && rt.RedirectURL != ""
}

func (a *App) SignOIDCFlow(flow OIDCFlow) (string, error) {
	payload, err := json.Marshal(flow)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, a.oidcStateKey[:])
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (a *App) VerifyOIDCFlow(value string) (OIDCFlow, error) {
	var flow OIDCFlow
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return flow, userErrf("invalid OIDC login state")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return flow, userErrf("invalid OIDC login state")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return flow, userErrf("invalid OIDC login state")
	}
	mac := hmac.New(sha256.New, a.oidcStateKey[:])
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) || json.Unmarshal(payload, &flow) != nil || time.Now().Unix() > flow.ExpiresAt {
		return OIDCFlow{}, userErrf("expired or invalid OIDC login state")
	}
	return flow, nil
}

func (a *App) BeginOIDC(ctx context.Context) (string, OIDCFlow, error) {
	rt, err := a.oidcRuntime(ctx)
	if err != nil {
		return "", OIDCFlow{}, err
	}
	if rt.Issuer == "" || rt.ClientID == "" || rt.RedirectURL == "" {
		return "", OIDCFlow{}, userErrf("OIDC is not fully configured")
	}
	provider, err := oidc.NewProvider(ctx, rt.Issuer)
	if err != nil {
		return "", OIDCFlow{}, fmt.Errorf("OIDC discovery failed: %w", err)
	}
	state, _ := token(24)
	nonce, _ := token(24)
	verifier := oauth2.GenerateVerifier()
	flow := OIDCFlow{State: state, Nonce: nonce, CodeVerifier: verifier, ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
	cfg := oauth2.Config{
		ClientID: rt.ClientID, ClientSecret: rt.ClientSecret, Endpoint: provider.Endpoint(),
		RedirectURL: rt.RedirectURL, Scopes: []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}
	authURL := cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	// Log exactly what we send so a Keycloak "Invalid Request" (which is almost
	// always a redirect_uri that doesn't match a registered Valid Redirect URI,
	// or a scope the client isn't allowed) can be compared against the client
	// config without guesswork.
	slog.Info("OIDC authorization request built",
		"issuer", rt.Issuer, "client_id", rt.ClientID, "redirect_uri", rt.RedirectURL,
		"scopes", cfg.Scopes)
	return authURL, flow, nil
}

type oidcClaims struct {
	Subject           string   `json:"sub"`
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	EmailVerified     bool     `json:"email_verified"`
	Name              string   `json:"name"`
	Groups            []string `json:"groups"`
	AuthorizedParty   string   `json:"azp"`
	Audience          any      `json:"aud"`
}

// oidcRole maps IdP group membership to a Postra role.
func oidcRole(groups []string, adminGroup string) domain.UserRole {
	if adminGroup == "" {
		return domain.RoleUser
	}
	for _, g := range groups {
		if strings.TrimPrefix(g, "/") == strings.TrimPrefix(adminGroup, "/") {
			return domain.RoleAdmin
		}
	}
	return domain.RoleUser
}

func (a *App) AuthenticateOIDCAccessToken(ctx context.Context, raw string) (domain.Principal, error) {
	rt, err := a.oidcRuntime(ctx)
	if err != nil || rt.Issuer == "" || rt.ClientID == "" {
		return domain.Principal{}, domain.ErrNotFound
	}
	provider, err := oidc.NewProvider(ctx, rt.Issuer)
	if err != nil {
		return domain.Principal{}, err
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: rt.ClientID, SkipClientIDCheck: true}).Verify(ctx, raw)
	if err != nil {
		return domain.Principal{}, domain.ErrNotFound
	}
	var claims oidcClaims
	if idToken.Claims(&claims) != nil || claims.Subject == "" || !tokenForClient(claims, rt.ClientID) {
		return domain.Principal{}, domain.ErrNotFound
	}
	u, err := a.Store.GetUserByOIDC(ctx, rt.Issuer, claims.Subject)
	if err != nil || u.Status != domain.UserActive {
		return domain.Principal{}, domain.ErrNotFound
	}
	return principalFor(u, "oidc_bearer"), nil
}

func tokenForClient(claims oidcClaims, clientID string) bool {
	if claims.AuthorizedParty == clientID {
		return true
	}
	switch aud := claims.Audience.(type) {
	case string:
		return aud == clientID
	case []any:
		for _, value := range aud {
			if value == clientID {
				return true
			}
		}
	}
	return false
}

func (a *App) CompleteOIDC(ctx context.Context, code string, flow OIDCFlow) (*domain.User, error) {
	rt, err := a.oidcRuntime(ctx)
	if err != nil {
		return nil, err
	}
	provider, err := oidc.NewProvider(ctx, rt.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}
	cfg := oauth2.Config{ClientID: rt.ClientID, ClientSecret: rt.ClientSecret, Endpoint: provider.Endpoint(),
		RedirectURL: rt.RedirectURL, Scopes: []string{oidc.ScopeOpenID, "profile", "email", "groups"}}
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(flow.CodeVerifier))
	if err != nil {
		// Surface the token endpoint's real reason. Keycloak returns
		// {"error":"invalid_request"/"invalid_client"/..., "error_description":"..."}
		// which oauth2 exposes as *oauth2.RetrieveError — the opaque generic
		// message hid exactly the detail needed to fix redirect_uri / client
		// secret / PKCE problems.
		detail := err.Error()
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			detail = fmt.Sprintf("%s: %s", re.ErrorCode, re.ErrorDescription)
			if re.ErrorCode == "" {
				detail = strings.TrimSpace(string(re.Body))
			}
		}
		slog.Warn("OIDC code exchange failed", "detail", detail, "redirect_url", rt.RedirectURL, "client_id", rt.ClientID)
		a.recordIncident(domain.SeverityError, "oidc", "OIDC 토큰 교환 실패", detail)
		return nil, userErrf("OIDC 코드 교환 실패: %s", detail)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, userErrf("OIDC provider returned no ID token")
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: rt.ClientID}).Verify(ctx, rawIDToken)
	if err != nil {
		slog.Warn("OIDC ID token verification failed", "detail", err.Error(), "client_id", rt.ClientID)
		a.recordIncident(domain.SeverityError, "oidc", "OIDC ID 토큰 검증 실패", err.Error())
		return nil, userErrf("OIDC ID 토큰 검증 실패: %s", err.Error())
	}
	var claims oidcClaims
	if err := idToken.Claims(&claims); err != nil || claims.Subject == "" {
		return nil, userErrf("OIDC ID token has invalid claims")
	}
	if idToken.Nonce != flow.Nonce {
		return nil, userErrf("OIDC nonce mismatch")
	}
	u, err := a.Store.GetUserByOIDC(ctx, rt.Issuer, claims.Subject)
	if err == nil {
		if u.Status != domain.UserActive {
			return nil, userErrf("user is disabled")
		}
		u.LastLoginAt = time.Now().Unix()
		_ = a.Store.UpdateUser(ctx, u)
		return u, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	// No (issuer,subject) match yet. Before treating this as a brand-new user,
	// adopt a pre-existing LOCAL account (e.g. the bootstrap admin) that this
	// person clearly owns, so IdP login uses that account instead of 401ing or
	// creating a duplicate. This runs even when auto-provision is off, because
	// linking an existing account is not provisioning a new one.
	if linked := a.linkExistingLocalUser(ctx, rt, claims); linked != nil {
		return linked, nil
	}

	if !rt.AutoProvision {
		return nil, userErrf("OIDC 사용자 자동 생성이 비활성화되어 있고, 연결할 기존 계정도 없습니다. 관리자에게 문의하세요 (사용자명/이메일이 기존 계정과 일치하는지 확인).")
	}
	loginID := strings.TrimSpace(claims.PreferredUsername)
	if loginID == "" {
		loginID = strings.TrimSpace(claims.Email)
	}
	if loginID == "" {
		loginID = "oidc-" + shortSubject(claims.Subject)
	}
	if _, _, err := a.Store.GetUserByLogin(ctx, loginID); err == nil {
		loginID += "-" + shortSubject(claims.Subject)
	}
	u = &domain.User{
		ID: persistence.NewID("usr"), LoginID: loginID, DisplayName: claims.Name, Email: claims.Email,
		Role: oidcRole(claims.Groups, rt.AdminGroup), Status: domain.UserActive, AuthProvider: "oidc",
		OIDCIssuer: rt.Issuer, OIDCSubject: claims.Subject, LastLoginAt: time.Now().Unix(),
	}
	if u.DisplayName == "" {
		u.DisplayName = loginID
	}
	if err := a.Store.CreateUser(ctx, u, ""); err != nil {
		return nil, err
	}
	a.audit(WithPrincipal(ctx, principalFor(u, "oidc")), "user_provision", "user:"+u.ID, "ok", rt.Issuer)
	return u, nil
}

// linkExistingLocalUser adopts a pre-existing LOCAL account for an OIDC login
// that has no (issuer,subject) match yet. It links only accounts that are safe
// to adopt: active, still local (not already federated to another IdP
// identity), and matched by a trustworthy signal. On success the OIDC identity
// is stored on that account so subsequent logins resolve directly, admin-group
// membership can elevate the role, and the account keeps working (password
// login stays available). Returns nil when nothing safe matches.
func (a *App) linkExistingLocalUser(ctx context.Context, rt oidcRuntime, claims oidcClaims) *domain.User {
	cand := a.oidcLinkCandidate(ctx, claims)
	if cand == nil || cand.Status != domain.UserActive {
		return nil
	}
	// Never hijack an account already federated to a (possibly different) IdP
	// identity, and never adopt anything but a local account.
	if cand.OIDCSubject != "" || cand.AuthProvider != "local" {
		return nil
	}
	cand.OIDCIssuer, cand.OIDCSubject = rt.Issuer, claims.Subject
	if cand.Email == "" {
		cand.Email = claims.Email
	}
	if oidcRole(claims.Groups, rt.AdminGroup) == domain.RoleAdmin {
		cand.Role = domain.RoleAdmin
	}
	cand.LastLoginAt = time.Now().Unix()
	if err := a.Store.UpdateUser(ctx, cand); err != nil {
		slog.Error("oidc: failed to link existing local account", "user", cand.ID, "err", err)
		return nil
	}
	a.audit(WithPrincipal(ctx, principalFor(cand, "oidc")), "user_oidc_link", "user:"+cand.ID, "ok",
		fmt.Sprintf("%s → login=%s", rt.Issuer, cand.LoginID))
	return cand
}

// oidcLinkCandidate finds the local account an OIDC identity should adopt:
// first a verified-email match, then preferred_username == login_id (which
// covers the emailless bootstrap admin) — but only when the token does not
// assert a *different* verified email for that username, to avoid takeover.
func (a *App) oidcLinkCandidate(ctx context.Context, claims oidcClaims) *domain.User {
	if claims.Email != "" && claims.EmailVerified {
		if u, err := a.Store.GetUserByEmail(ctx, claims.Email); err == nil && u != nil {
			return u
		}
	}
	login := strings.TrimSpace(claims.PreferredUsername)
	if login == "" {
		return nil
	}
	u, _, err := a.Store.GetUserByLogin(ctx, login)
	if err != nil || u == nil {
		return nil
	}
	if u.Email != "" && claims.Email != "" && !strings.EqualFold(u.Email, claims.Email) {
		return nil // username matches but emails disagree — refuse to link
	}
	return u
}

func shortSubject(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}

func ValidateOIDCRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return userErrf("OIDC redirect URL must be absolute")
	}
	if u.Scheme != "https" && !strings.HasPrefix(u.Host, "127.0.0.1") && !strings.HasPrefix(u.Host, "localhost") {
		return userErrf("OIDC redirect URL must use HTTPS outside loopback")
	}
	return nil
}
