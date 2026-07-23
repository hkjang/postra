package application

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	rt := oidcRuntime{
		Issuer: values[SettingOIDCIssuer], ClientID: values[SettingOIDCClientID],
		SecretRef: values[SettingOIDCSecretRef], RedirectURL: values[SettingOIDCRedirectURL],
		AdminGroup:    values[SettingOIDCAdminGroup],
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
	return authURL, flow, nil
}

type oidcClaims struct {
	Subject           string   `json:"sub"`
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	Groups            []string `json:"groups"`
	AuthorizedParty   string   `json:"azp"`
	Audience          any      `json:"aud"`
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
		return nil, userErrf("OIDC code exchange failed")
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, userErrf("OIDC provider returned no ID token")
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: rt.ClientID}).Verify(ctx, rawIDToken)
	if err != nil {
		return nil, userErrf("OIDC ID token verification failed")
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
	if !rt.AutoProvision {
		return nil, userErrf("OIDC user provisioning is disabled; contact an administrator")
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
	role := domain.RoleUser
	for _, group := range claims.Groups {
		if strings.TrimPrefix(group, "/") == strings.TrimPrefix(rt.AdminGroup, "/") && rt.AdminGroup != "" {
			role = domain.RoleAdmin
			break
		}
	}
	u = &domain.User{
		ID: persistence.NewID("usr"), LoginID: loginID, DisplayName: claims.Name, Email: claims.Email,
		Role: role, Status: domain.UserActive, AuthProvider: "oidc",
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
