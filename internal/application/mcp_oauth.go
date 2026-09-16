package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"postra/internal/domain"
)

// These fields are connection instructions, not credentials or a health probe.
type MCPOAuthInfo struct {
	Enabled          bool     `json:"enabled"`
	Configured       bool     `json:"configured"`
	Issuer           string   `json:"issuer"`
	ResourceURL      string   `json:"resource_url"`
	MetadataURL      string   `json:"metadata_url"`
	AllowedClientIDs []string `json:"allowed_client_ids"`
	ScopesSupported  []string `json:"scopes_supported"`
}
type MCPOAuthConnectionInfo struct {
	ActiveEndpoint     string       `json:"active_endpoint"`
	ConfiguredEndpoint string       `json:"configured_endpoint"`
	PendingRestart     bool         `json:"pending_restart"`
	OAuth              MCPOAuthInfo `json:"oauth"`
}

func oauthList(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
}

func validOAuthURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" || u.RawPath != "" || strings.ContainsAny(raw, "\\ \t\r\n#%") {
		return false
	}
	if u.Path != "" && strings.TrimSuffix(u.Path, "/") != path.Clean(u.Path) {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname())
}

func validateMCPOAuthSetting(key, value string) error {
	switch key {
	case "mcp.oauth.resource_url":
		if value != "" && !validOAuthURL(value) {
			return userErrf("OAuth MCP 공개 URL은 인증정보·쿼리·프래그먼트 없는 HTTPS URL이어야 합니다 (localhost 개발 예외)")
		}
	case "mcp.oauth.allowed_client_ids":
		ids := oauthList(value)
		if len(ids) > 50 {
			return userErrf("OAuth Client는 최대 50개까지 허용할 수 있습니다")
		}
		for _, id := range ids {
			if len(id) > 128 {
				return userErrf("OAuth Client ID가 너무 깁니다")
			}
			for _, r := range id {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
					return userErrf("OAuth Client ID는 영문·숫자 및 . _ : - 문자만 사용할 수 있습니다")
				}
			}
		}
	case "mcp.oauth.allowed_scopes":
		if _, err := NormalizeMCPScopes(oauthList(value)); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) oauthSettingsSnapshot() map[string]string {
	a.runtimeSettings.RLock()
	defer a.runtimeSettings.RUnlock()
	out := make(map[string]string, len(a.runtimeSettings.values))
	for k, v := range a.runtimeSettings.values {
		out[k] = v
	}
	return out
}

func oauthConfigured(values map[string]string, endpoint string) bool {
	resource, err := url.Parse(values["mcp.oauth.resource_url"])
	return err == nil && validOAuthURL(values["mcp.oauth.resource_url"]) && resource.Path == endpoint &&
		validOAuthURL(values[SettingOIDCIssuer]) && len(oauthList(values["mcp.oauth.allowed_client_ids"])) > 0 &&
		!slices.Contains(oauthList(values["mcp.oauth.allowed_client_ids"]), values[SettingOIDCClientID]) &&
		validateMCPOAuthSetting("mcp.oauth.allowed_client_ids", values["mcp.oauth.allowed_client_ids"]) == nil &&
		validateMCPOAuthSetting("mcp.oauth.allowed_scopes", values["mcp.oauth.allowed_scopes"]) == nil
}

func (a *App) validateMCPOAuthSettings(stored, changes map[string]string) error {
	relevant := false
	for key := range changes {
		if strings.HasPrefix(key, "mcp.oauth.") || key == "mcp.endpoint" || key == SettingOIDCIssuer || key == SettingOIDCClientID {
			relevant = true
		}
	}
	if !relevant {
		return nil
	}
	values := map[string]string{}
	for _, d := range SettingsDefinitions() {
		value, _ := a.initialSetting(d)
		values[d.Key] = value
	}
	for k, v := range stored {
		values[k] = v
	}
	for k, v := range changes {
		values[k] = v
	}
	if values["mcp.oauth.enabled"] == "true" && !oauthConfigured(values, values["mcp.endpoint"]) {
		return userErrf("OAuth MCP 활성화에는 HTTPS Keycloak Issuer, MCP Endpoint 경로와 일치하는 공개 URL, 웹 SSO와 구분된 허용 Client ID 및 유효한 Scope가 필요합니다")
	}
	return nil
}

// MCPOAuthConnection never derives security URLs from untrusted Host headers.
func (a *App) MCPOAuthConnection() MCPOAuthConnectionInfo {
	values := a.oauthSettingsSnapshot()
	active := a.startedMCPEndpoint
	if active == "" {
		active = "/mcp"
	}
	o := MCPOAuthInfo{Enabled: values["mcp.oauth.enabled"] == "true", Configured: oauthConfigured(values, active),
		Issuer: values[SettingOIDCIssuer], ResourceURL: values["mcp.oauth.resource_url"],
		AllowedClientIDs: append([]string{}, oauthList(values["mcp.oauth.allowed_client_ids"])...), ScopesSupported: []string{}}
	// Deployment/imported values can predate current setting validation. Do
	// not expose malformed URLs (potentially containing credentials) to users.
	if !validOAuthURL(o.Issuer) {
		o.Issuer = ""
	}
	if !validOAuthURL(o.ResourceURL) {
		o.ResourceURL = ""
	}
	if validOAuthURL(o.ResourceURL) {
		u, _ := url.Parse(o.ResourceURL)
		u.Path = "/.well-known/oauth-protected-resource" + u.Path
		o.MetadataURL = u.String()
	}
	for _, scope := range oauthList(values["mcp.oauth.allowed_scopes"]) {
		permission := "mcp.permissions." + strings.ReplaceAll(strings.TrimPrefix(scope, "mail."), ".", "_")
		if slices.Contains(MCPScopes, scope) && values[permission] == "true" && !slices.Contains(o.ScopesSupported, scope) {
			o.ScopesSupported = append(o.ScopesSupported, scope)
		}
	}
	return MCPOAuthConnectionInfo{ActiveEndpoint: active, ConfiguredEndpoint: values["mcp.endpoint"], PendingRestart: active != values["mcp.endpoint"], OAuth: o}
}

type mcpOAuthProviderCache struct {
	sync.Mutex
	issuer   string
	provider *oidc.Provider
	expires  time.Time
	loading  chan struct{}
}

// Bound discovery/JWKS work and body sizes; never forward client credentials,
// follow redirects, use token-provided key URLs, or disable TLS verification.
type oauthTransport struct {
	base        http.RoundTripper
	issuer      string
	mu          sync.Mutex
	nextRefresh time.Time
}

func (t *oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// go-oidc coalesces concurrent key refreshes but deliberately retries on
	// every unknown kid. Bound sequential forged-token traffic to one JWKS
	// refresh per second, without caching authorization decisions or blocking
	// requests verified by already cached keys. Rotation may wait at most 1s.
	if r.URL.String() != strings.TrimSuffix(t.issuer, "/")+"/.well-known/openid-configuration" {
		t.mu.Lock()
		delay := time.Until(t.nextRefresh)
		if delay < 0 {
			delay = 0
		}
		t.nextRefresh = time.Now().Add(delay + time.Second)
		t.mu.Unlock()
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-timer.C:
			}
		}
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > 1<<20 {
		response.Body.Close()
		return nil, errors.New("OAuth metadata too large")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, errors.New("OAuth metadata too large")
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

func (a *App) mcpProvider(ctx context.Context, issuer string) (*oidc.Provider, error) {
	c := &a.mcpOAuthProvider
	for {
		c.Lock()
		if c.issuer == issuer && time.Now().Before(c.expires) {
			p := c.provider
			c.Unlock()
			if p == nil {
				return nil, domain.ErrNotFound
			}
			return p, nil
		}
		if c.loading != nil {
			wait := c.loading
			c.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
				continue
			}
		}
		c.loading = make(chan struct{})
		c.Unlock()
		client := &http.Client{Timeout: 10 * time.Second, Transport: &oauthTransport{base: http.DefaultTransport, issuer: issuer}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
		if err == nil {
			var metadata struct {
				JWKSURI string `json:"jwks_uri"`
			}
			if p.Claims(&metadata) != nil || !validOAuthURL(metadata.JWKSURI) {
				err = domain.ErrNotFound
			} else {
				keys, _ := url.Parse(metadata.JWKSURI)
				source, _ := url.Parse(issuer)
				if keys.Scheme != source.Scheme || keys.Host != source.Host {
					err = domain.ErrNotFound
				}
			}
		}
		c.Lock()
		c.issuer = issuer
		c.provider = p
		c.expires = time.Now().Add(5 * time.Minute)
		if err != nil {
			c.provider = nil
			c.expires = time.Now().Add(5 * time.Second)
		}
		close(c.loading)
		c.loading = nil
		c.Unlock()
		if err != nil {
			return nil, domain.ErrNotFound
		}
		return p, nil
	}
}

type keycloakAccessClaims struct {
	Type         string          `json:"typ"`
	ClientID     string          `json:"azp"`
	Scope        string          `json:"scope"`
	Expires      int64           `json:"exp"`
	NotBefore    int64           `json:"nbf"`
	Confirmation json.RawMessage `json:"cnf"`
}

func (c keycloakAccessClaims) valid() bool {
	now := time.Now().Unix()
	return c.Type == "Bearer" && c.Expires > now && c.NotBefore <= now && len(c.Confirmation) == 0
}

// AuthenticateMCPOAuthToken accepts only a delegated Keycloak access token for
// this MCP resource. Web login/ID tokens are not MCP credentials. No email
// linking, provisioning, role promotion or credential persistence occurs here.
func (a *App) AuthenticateMCPOAuthToken(ctx context.Context, raw string) (domain.Principal, error) {
	fail := func() (domain.Principal, error) { return domain.Principal{}, domain.ErrNotFound }
	if len(raw) > 32768 || strings.Count(raw, ".") != 2 {
		return fail()
	}
	o := a.MCPOAuthConnection().OAuth
	if !o.Enabled || !o.Configured || !a.SettingBool("mcp.enabled") || !a.SettingBool("mcp.http_enabled") {
		return fail()
	}
	p, err := a.mcpProvider(ctx, o.Issuer)
	if err != nil {
		return fail()
	}
	token, err := p.Verifier(&oidc.Config{ClientID: o.ResourceURL, SupportedSigningAlgs: []string{oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512, oidc.PS256, oidc.PS384, oidc.PS512}}).Verify(ctx, raw)
	if err != nil || strings.TrimSpace(token.Subject) == "" {
		return fail()
	}
	var claims keycloakAccessClaims
	if token.Claims(&claims) != nil || !claims.valid() || !slices.Contains(o.AllowedClientIDs, claims.ClientID) {
		return fail()
	}
	user, err := a.Store.GetUserByOIDC(ctx, o.Issuer, token.Subject)
	if err != nil || user.Status != domain.UserActive {
		return fail()
	}
	principal := principalFor(user, "mcp_oauth")
	principal.OAuthClientID = claims.ClientID
	principal.OAuthIssuer = o.Issuer
	principal.OAuthExpiresAt = token.Expiry
	principal.MCPScopes = []string{}
	for _, scope := range strings.Fields(claims.Scope) {
		if slices.Contains(o.ScopesSupported, scope) && !slices.Contains(principal.MCPScopes, scope) {
			principal.MCPScopes = append(principal.MCPScopes, scope)
		}
	}
	return principal, nil
}
