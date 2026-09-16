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
	"unicode/utf8"

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
	// Silent records that this flow was started with prompt=none, so the
	// callback can tell a routine login_required answer from a real failure.
	Silent bool `json:"silent,omitempty"`
	// ReturnTo is the in-app path to land on after login (deep links).
	ReturnTo string `json:"return_to,omitempty"`
}

// OIDCStartOptions shapes one authorization request.
type OIDCStartOptions struct {
	// Silent asks for prompt=none: the provider answers from an existing
	// session only and never renders UI. Honoured only when the administrator
	// has enabled auto_login; otherwise it is downgraded to a normal login.
	Silent   bool
	ReturnTo string
}

type oidcRuntime struct {
	Issuer, ClientID, ClientSecret, SecretRef, RedirectURL, AdminGroup string
	AutoProvision, AutoLogin                                           bool
}

// OIDCDefaultReturnTo is where a login lands when no safe return_to was given.
const OIDCDefaultReturnTo = "/app/"

// SafeReturnTo accepts only in-app paths: it must start with "/" and must not
// start with "//" (a scheme-relative URL), so the login flow can never be used
// as a springboard to another site.
func SafeReturnTo(value string) bool {
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") ||
		strings.HasPrefix(value, "/\\") || strings.ContainsAny(value, "\r\n") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && !parsed.IsAbs() && parsed.Host == ""
}

// OIDCReturnToOrDefault returns value when it is a safe in-app path, else the
// default landing page.
func OIDCReturnToOrDefault(value string) string {
	if SafeReturnTo(value) {
		return value
	}
	return OIDCDefaultReturnTo
}

// OIDCLoginRequired reports whether an authorization error is the provider's
// ordinary "no session" answer to prompt=none rather than a misconfiguration.
func OIDCLoginRequired(code string) bool {
	switch code {
	case "login_required", "interaction_required", "consent_required":
		return true
	}
	return false
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
		AutoLogin:     boolSetting(values, SettingOIDCAutoLogin, a.Cfg.Auth.OIDCAutoLogin),
	}
	if rt.SecretRef != "" {
		if a.Secrets == nil {
			return rt, errors.New("OIDC secret store unavailable")
		}
		handle, err := a.Secrets.Acquire(ctx, domain.SecretRef(rt.SecretRef), domain.PurposeOIDC)
		if err != nil {
			return rt, err
		}
		rt.ClientSecret = string(handle.Reveal())
		handle.Zero()
	} else {
		// A persisted empty reference is an explicit administrator clear,
		// not permission to resurrect a deployment's old plaintext secret.
		// Read persisted presence directly so external replica updates apply
		// consistently even before the background settings cache refreshes.
		stored, err := a.Store.GetSettings(ctx)
		if err != nil {
			return rt, err
		}
		if _, explicit := stored[SettingOIDCSecretRef]; !explicit {
			rt.ClientSecret = a.Cfg.Auth.OIDCClientSecret
		}
	}
	return rt, nil
}

func (a *App) OIDCConfigured(ctx context.Context) bool {
	rt, err := a.oidcRuntime(ctx)
	return err == nil && rt.configured()
}

func (rt oidcRuntime) configured() bool {
	return rt.Issuer != "" && rt.ClientID != "" && rt.RedirectURL != ""
}

// OIDCAutoLoginEnabled reports whether the browser may try a silent
// (prompt=none) sign-in: OIDC must be fully configured and the administrator
// must have switched auto_login on.
func (a *App) OIDCAutoLoginEnabled(ctx context.Context) bool {
	rt, err := a.oidcRuntime(ctx)
	return err == nil && rt.configured() && rt.AutoLogin
}

func (a *App) SignOIDCFlow(flow OIDCFlow) (string, error) {
	payload, err := json.Marshal(flow)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(a.oidcMAC("", payload)), nil
}

func (a *App) VerifyOIDCFlow(value string) (OIDCFlow, error) {
	var flow OIDCFlow
	payload, ok := a.verifyOIDCSigned("", value)
	if !ok {
		return flow, userErrf("invalid OIDC login state")
	}
	if json.Unmarshal(payload, &flow) != nil || time.Now().Unix() > flow.ExpiresAt {
		return OIDCFlow{}, userErrf("expired or invalid OIDC login state")
	}
	return flow, nil
}

// OIDCErrorNoteTTL bounds how long a failed sign-in's reason waits to be shown.
const OIDCErrorNoteTTL = time.Minute

// oidcErrorNote is the one-shot note the callback leaves for the login page
// when a sign-in fails: the reason is shown at /app/login?sso=error instead of
// on the callback address, where a refresh would resubmit the spent code.
type oidcErrorNote struct {
	Message   string `json:"message"`
	ExpiresAt int64  `json:"expires_at"`
}

// oidcErrorDomain keeps error notes and flow cookies from verifying as each
// other even though they share the state key.
const oidcErrorDomain = "oidc-error:"

// oidcErrorNoteMaxBytes keeps the note well inside a browser's cookie limit
// even when a provider pastes its whole response body into the reason.
const oidcErrorNoteMaxBytes = 1024

// SignOIDCError signs a failure reason so the login page can trust that it was
// written by this server and not pasted in by whoever set a cookie.
func (a *App) SignOIDCError(message string) (string, error) {
	if len(message) > oidcErrorNoteMaxBytes {
		cut := oidcErrorNoteMaxBytes
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut] + "…"
	}
	payload, err := json.Marshal(oidcErrorNote{Message: message, ExpiresAt: time.Now().Add(OIDCErrorNoteTTL).Unix()})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(a.oidcMAC(oidcErrorDomain, payload)), nil
}

// VerifyOIDCError returns the failure reason carried by a note from
// SignOIDCError, or false when the note is missing, forged or stale.
func (a *App) VerifyOIDCError(value string) (string, bool) {
	payload, ok := a.verifyOIDCSigned(oidcErrorDomain, value)
	if !ok {
		return "", false
	}
	var note oidcErrorNote
	if json.Unmarshal(payload, &note) != nil || note.Message == "" || time.Now().Unix() > note.ExpiresAt {
		return "", false
	}
	return note.Message, true
}

func (a *App) oidcMAC(scope string, payload []byte) []byte {
	mac := hmac.New(sha256.New, a.oidcStateKey[:])
	mac.Write([]byte(scope))
	mac.Write(payload)
	return mac.Sum(nil)
}

func (a *App) verifyOIDCSigned(scope, value string) ([]byte, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	if !hmac.Equal(sig, a.oidcMAC(scope, payload)) {
		return nil, false
	}
	return payload, true
}

func (a *App) BeginOIDC(ctx context.Context, opts OIDCStartOptions) (string, OIDCFlow, error) {
	rt, err := a.oidcRuntime(ctx)
	if err != nil {
		return "", OIDCFlow{}, err
	}
	if !rt.configured() {
		return "", OIDCFlow{}, userErrf("OIDC is not fully configured")
	}
	provider, err := oidc.NewProvider(ctx, rt.Issuer)
	if err != nil {
		return "", OIDCFlow{}, userErrf("OIDC Discovery 실패: Issuer URL, 인증 서버 연결과 TLS 인증서를 확인하세요")
	}
	// A silent attempt is only ever what the administrator allowed. Anyone can
	// append ?prompt=none to the start URL; without auto_login it is quietly
	// treated as an ordinary login so the redirect surface stays tied to the
	// setting.
	silent := opts.Silent && rt.AutoLogin
	state, _ := token(24)
	nonce, _ := token(24)
	verifier := oauth2.GenerateVerifier()
	flow := OIDCFlow{State: state, Nonce: nonce, CodeVerifier: verifier, ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Silent: silent, ReturnTo: OIDCReturnToOrDefault(opts.ReturnTo)}
	cfg := oauth2.Config{
		ClientID: rt.ClientID, ClientSecret: rt.ClientSecret, Endpoint: provider.Endpoint(),
		RedirectURL: rt.RedirectURL, Scopes: []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}
	authOpts := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)}
	if silent {
		authOpts = append(authOpts, oauth2.SetAuthURLParam("prompt", "none"))
	}
	authURL := cfg.AuthCodeURL(state, authOpts...)
	// Log exactly what we send so a Keycloak "Invalid Request" (which is almost
	// always a redirect_uri that doesn't match a registered Valid Redirect URI,
	// or a scope the client isn't allowed) can be compared against the client
	// config without guesswork.
	slog.Info("OIDC authorization request built",
		"issuer", rt.Issuer, "client_id", rt.ClientID, "redirect_uri", rt.RedirectURL,
		"scopes", cfg.Scopes, "silent", silent)
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
	idToken, err := provider.Verifier(&oidc.Config{ClientID: rt.ClientID}).Verify(ctx, raw)
	if err != nil {
		return domain.Principal{}, domain.ErrNotFound
	}
	var claims oidcClaims
	if idToken.Claims(&claims) != nil || claims.Subject == "" || !tokenForClient(claims, rt.ClientID) {
		return domain.Principal{}, domain.ErrNotFound
	}
	// This legacy REST credential is distinct from delegated MCP OAuth.
	// In particular, never fall back to unrestricted user authority when a
	// resource token fails the MCP verifier or contains multiple audiences.
	var access keycloakAccessClaims
	if idToken.Claims(&access) != nil || !access.valid() {
		return domain.Principal{}, domain.ErrNotFound
	}
	for _, scope := range strings.Fields(access.Scope) {
		if contains(MCPScopes, scope) {
			return domain.Principal{}, domain.ErrNotFound
		}
	}
	oauth := a.MCPOAuthConnection().OAuth
	if contains(oauth.AllowedClientIDs, access.ClientID) || oauth.ResourceURL != "" && contains(idToken.Audience, oauth.ResourceURL) {
		return domain.Principal{}, domain.ErrNotFound
	}
	u, err := a.Store.GetUserByOIDC(ctx, rt.Issuer, claims.Subject)
	if err != nil || u.Status != domain.UserActive {
		return domain.Principal{}, domain.ErrNotFound
	}
	return principalFor(u, "oidc_bearer"), nil
}

func tokenForClient(claims oidcClaims, clientID string) bool {
	if claims.AuthorizedParty != clientID {
		return false
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
		return nil, userErrf("OIDC Discovery 실패: Issuer URL, 인증 서버 연결과 TLS 인증서를 확인하세요")
	}
	cfg := oauth2.Config{ClientID: rt.ClientID, ClientSecret: rt.ClientSecret, Endpoint: provider.Endpoint(),
		RedirectURL: rt.RedirectURL, Scopes: []string{oidc.ScopeOpenID, "profile", "email", "groups"}}
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(flow.CodeVerifier))
	if err != nil {
		// Provider descriptions, raw bodies and even unknown error codes may
		// echo a client secret, authorization code or token. Only known codes
		// and our own actionable guidance can cross the diagnostic boundary.
		detail := oidcExchangeGuidance(err)
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
		const detail = "Issuer, Client ID(audience), 서명/JWKS, 서버 시간과 토큰 만료를 확인한 뒤 다시 로그인하세요"
		slog.Warn("OIDC ID token verification failed", "detail", detail, "client_id", rt.ClientID)
		a.recordIncident(domain.SeverityError, "oidc", "OIDC ID 토큰 검증 실패", detail)
		return nil, userErrf("OIDC ID 토큰 검증 실패: %s", detail)
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
		if u.Status == domain.UserDeleted {
			// Compatibility with users deleted before identity tombstoning:
			// release the old subject/email, then continue as a fresh login.
			tombstoneUserIdentity(u)
			if updateErr := a.Store.UpdateUser(ctx, u); updateErr != nil {
				return nil, updateErr
			}
			err = domain.ErrNotFound
		} else {
			if u.Status != domain.UserActive {
				return nil, userErrf("user is disabled")
			}
			u.LastLoginAt = time.Now().Unix()
			_ = a.Store.UpdateUser(ctx, u)
			if strings.TrimSpace(claims.Email) != "" {
				a.tryAutoProvisionOIDCMail(ctx, u)
			}
			return u, nil
		}
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
		if strings.TrimSpace(claims.Email) != "" {
			a.tryAutoProvisionOIDCMail(ctx, linked)
		}
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
	if strings.TrimSpace(claims.Email) != "" {
		a.tryAutoProvisionOIDCMail(ctx, u)
	}
	return u, nil
}

func oidcExchangeGuidance(err error) string {
	var endpointError *oauth2.RetrieveError
	if errors.As(err, &endpointError) {
		return OIDCProviderErrorGuidance(endpointError.ErrorCode)
	}
	return OIDCProviderErrorGuidance("")
}

// OIDCProviderErrorGuidance is shared with the authorization callback. Never
// pass through provider descriptions, unknown codes, URLs or token contents.
func OIDCProviderErrorGuidance(code string) string {
	guidance := ""
	switch code {
	case "invalid_client":
		guidance = "Client ID, 별도 저장된 Client Secret과 인증 서버의 클라이언트 인증 방식을 확인하세요"
	case "invalid_grant":
		guidance = "인증 코드가 만료되었거나 이미 사용되었을 수 있습니다. 다시 로그인하고 Callback URL과 PKCE 설정을 확인하세요"
	case "invalid_request":
		guidance = "Callback URL, PKCE와 인증 서버의 클라이언트 요청 설정을 확인하세요"
	case "invalid_scope":
		guidance = "openid, profile, email, groups 범위가 해당 클라이언트에 허용되는지 확인하세요"
	case "unauthorized_client", "unsupported_grant_type", "unsupported_response_type":
		guidance = "클라이언트의 Authorization Code 흐름과 토큰 발급 권한을 확인하세요"
	case "access_denied":
		guidance = "사용자와 클라이언트의 로그인 권한을 확인하세요"
	case "login_required", "interaction_required", "consent_required", "account_selection_required":
		guidance = "인증 서버에서 로그인·계정 선택·동의를 완료한 뒤 다시 시도하세요"
	case "server_error", "temporarily_unavailable":
		guidance = "인증 서버 상태를 확인한 뒤 잠시 후 다시 로그인하세요"
	}
	if guidance != "" {
		return code + ": " + guidance
	}
	return "인증 서버 연결, Client 설정과 Callback URL을 확인한 뒤 다시 로그인하세요"
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
	// Never hijack an account already federated to another identity. In
	// addition to local accounts, accept an administrator-preprovisioned OIDC
	// account whose issuer matches and whose subject has not been bound yet.
	preprovisioned := cand.AuthProvider == "oidc" && cand.OIDCSubject == "" &&
		cand.OIDCIssuer == rt.Issuer
	if cand.OIDCSubject != "" || (cand.AuthProvider != "local" && !preprovisioned) {
		return nil
	}
	if preprovisioned && (cand.Email == "" || !strings.EqualFold(cand.Email, claims.Email)) {
		return nil // pending OIDC identities bind only through the exact asserted email
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
// first an IdP-asserted email match, then preferred_username == login_id (which
// covers the emailless bootstrap admin) — but only when the token does not
// assert a *different* verified email for that username, to avoid takeover.
func (a *App) oidcLinkCandidate(ctx context.Context, claims oidcClaims) *domain.User {
	if claims.Email != "" {
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
