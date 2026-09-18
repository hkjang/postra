package application

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"postra/internal/domain"
)

// The DCR-compatible proxy lets MCP clients that only support dynamic client
// registration (RFC 7591) reach Keycloak through Postra: Postra is the
// authorization server the client sees, while Keycloak still authenticates
// the user through one pre-registered "proxy" client. Access tokens are the
// Keycloak tokens themselves, so the resource-side checks in mcp_oauth.go are
// unchanged; only the authorization code and refresh token handed to the
// client are Postra-sealed envelopes bound to that client.
const (
	SettingMCPOAuthProxyEnabled   = "mcp.oauth.proxy.enabled"
	SettingMCPOAuthProxyClientID  = "mcp.oauth.proxy.client_id"
	SettingMCPOAuthProxySecretRef = "mcp.oauth.proxy.client_secret_ref" // #nosec G101 -- encrypted-secret reference setting key

	OAuthProxyMetadataPath  = "/.well-known/oauth-authorization-server"
	OAuthProxyOpenIDPath    = "/.well-known/openid-configuration"
	OAuthProxyRegisterPath  = "/oauth/register"
	OAuthProxyAuthorizePath = "/oauth/authorize"
	OAuthProxyCallbackPath  = "/oauth/callback"
	OAuthProxyTokenPath     = "/oauth/token"

	oauthProxyStateTTL      = 10 * time.Minute
	oauthProxyCodeTTL       = 90 * time.Second
	oauthProxyClientMaxIdle = 30 * 24 * time.Hour
	oauthProxyClientLimit   = 500
)

// MCPOAuthProxyInfo is public connection guidance for the DCR proxy. It never
// carries the upstream client secret or any token.
type MCPOAuthProxyInfo struct {
	Enabled               bool   `json:"enabled"`
	Configured            bool   `json:"configured"`
	Issuer                string `json:"issuer"`
	MetadataURL           string `json:"metadata_url"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	CallbackURL           string `json:"callback_url"`
	ClientID              string `json:"client_id"`
}

// MCPOAuthProxyError is an RFC 6749 error. RedirectURI is set only once the
// client and its redirect URI were validated, so the transport may send the
// browser back to the client; otherwise it must render the error itself.
type MCPOAuthProxyError struct {
	Code        string
	Description string
	Status      int
	RedirectURI string
	State       string
}

func (e *MCPOAuthProxyError) Error() string { return e.Code + ": " + e.Description }

func oauthErr(status int, code, description string) *MCPOAuthProxyError {
	return &MCPOAuthProxyError{Code: code, Description: description, Status: status}
}

func oauthProxyClientID(values map[string]string) string {
	id := strings.TrimSpace(values[SettingMCPOAuthProxyClientID])
	if validateMCPOAuthSetting("mcp.oauth.allowed_client_ids", id) != nil {
		return ""
	}
	return id
}

// oauthAllowedClients is the effective allow list: the operator's
// pre-registered clients plus the proxy's own upstream client.
func oauthAllowedClients(values map[string]string) []string {
	ids := oauthList(values["mcp.oauth.allowed_client_ids"])
	if proxy := oauthProxyClientID(values); proxy != "" && !slices.Contains(ids, proxy) {
		ids = append(ids, proxy)
	}
	return ids
}

func oauthProxyOrigin(resourceURL string) string {
	if !validOAuthURL(resourceURL) {
		return ""
	}
	u, _ := url.Parse(resourceURL)
	return u.Scheme + "://" + u.Host
}

func oauthProxyConfigured(values map[string]string) bool {
	return oauthProxyClientID(values) != "" && oauthProxyClientID(values) != values[SettingOIDCClientID] && oauthProxyOrigin(values["mcp.oauth.resource_url"]) != ""
}

func oauthProxyInfo(values map[string]string, oauthConfigured bool) MCPOAuthProxyInfo {
	info := MCPOAuthProxyInfo{Enabled: values[SettingMCPOAuthProxyEnabled] == "true", ClientID: oauthProxyClientID(values)}
	origin := oauthProxyOrigin(values["mcp.oauth.resource_url"])
	if origin == "" {
		return info
	}
	info.Configured = oauthConfigured && oauthProxyConfigured(values)
	info.Issuer = origin
	info.MetadataURL = origin + OAuthProxyMetadataPath
	info.RegistrationEndpoint = origin + OAuthProxyRegisterPath
	info.AuthorizationEndpoint = origin + OAuthProxyAuthorizePath
	info.TokenEndpoint = origin + OAuthProxyTokenPath
	info.CallbackURL = origin + OAuthProxyCallbackPath
	return info
}

// MCPOAuthProxy reports whether the proxy accepts requests right now.
func (a *App) MCPOAuthProxy() (MCPOAuthInfo, bool) {
	o := a.MCPOAuthConnection().OAuth
	return o, o.Enabled && o.Configured && o.Proxy.Enabled && o.Proxy.Configured && a.SettingBool("mcp.enabled") && a.SettingBool("mcp.http_enabled")
}

// ---------- sealed envelopes (state, code, refresh token) ----------

func (a *App) oauthProxyAEAD() (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, a.oidcStateKey[:], nil, "postra-mcp-oauth-proxy", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealOAuthProxy encrypts v under the replica-shared key; kind is bound as
// associated data so a state can never be replayed as a code or refresh token.
func (a *App) sealOAuthProxy(kind string, v any) (string, error) {
	aead, err := a.oauthProxyAEAD()
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(kind))), nil
}

func (a *App) openOAuthProxy(kind, raw string, v any) error {
	if len(raw) > 16384 {
		return errors.New("envelope too large")
	}
	aead, err := a.oauthProxyAEAD()
	if err != nil {
		return err
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) < aead.NonceSize() {
		return errors.New("malformed envelope")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(kind))
	if err != nil {
		return errors.New("invalid envelope")
	}
	return json.Unmarshal(plain, v)
}

type oauthProxyState struct {
	ClientID      string   `json:"c"`
	RedirectURI   string   `json:"r"`
	State         string   `json:"s"`
	CodeChallenge string   `json:"h"`
	Scopes        []string `json:"sc"`
	Verifier      string   `json:"v"`
	Nonce         string   `json:"n"`
	ExpiresAt     int64    `json:"e"`
}

type oauthProxyCode struct {
	ClientID      string   `json:"c"`
	RedirectURI   string   `json:"r"`
	CodeChallenge string   `json:"h"`
	Scopes        []string `json:"sc"`
	UpstreamCode  string   `json:"uc"`
	Verifier      string   `json:"v"`
	Nonce         string   `json:"n"`
	ExpiresAt     int64    `json:"e"`
}

type oauthProxyRefresh struct {
	ClientID        string `json:"c"`
	UpstreamRefresh string `json:"rt"`
}

// ---------- dynamic client registration ----------

type MCPOAuthClientRegistration struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	Scope                   string   `json:"scope"`
}

// MCPOAuthClientRegistered is the RFC 7591 response; ClientSecret is shown
// exactly once and only its hash is stored.
type MCPOAuthClientRegistered struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientSecretExpiresAt   int64    `json:"client_secret_expires_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope,omitempty"`
}

func isLoopbackHost(host string) bool {
	return slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, strings.ToLower(host))
}

// validOAuthRedirectURI accepts HTTPS, loopback HTTP (any port, RFC 8252
// §7.3) and private-use schemes of native apps. Browser-executable and
// credential-bearing URLs are refused.
func validOAuthRedirectURI(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "#\\ \t\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Hostname() != ""
	case "http":
		return isLoopbackHost(u.Hostname())
	case "javascript", "data", "vbscript", "file", "blob", "about", "ftp":
		return false
	}
	for _, r := range u.Scheme {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '+') {
			return false
		}
	}
	return true
}

// redirectURIMatches compares a request URI against a registered one; only
// loopback HTTP redirects may vary in port, as native apps pick one at start.
func redirectURIMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	a, errA := url.Parse(registered)
	b, errB := url.Parse(requested)
	if errA != nil || errB != nil || a.Scheme != "http" || b.Scheme != "http" || !isLoopbackHost(a.Hostname()) || a.Hostname() != b.Hostname() {
		return false
	}
	return a.Path == b.Path && a.RawQuery == b.RawQuery && a.User == nil && b.User == nil && b.Fragment == ""
}

func cleanClientName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// RegisterMCPOAuthClient stores an anonymous RFC 7591 registration. A
// registration only names redirect targets; it grants no user or mail access.
func (a *App) RegisterMCPOAuthClient(ctx context.Context, req MCPOAuthClientRegistration) (*MCPOAuthClientRegistered, error) {
	if _, ok := a.MCPOAuthProxy(); !ok {
		return nil, oauthErr(http.StatusNotFound, "invalid_request", "dynamic client registration is not enabled")
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		return nil, oauthErr(http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris must list 1 to 10 HTTPS, loopback or native-app URIs")
	}
	uris := []string{}
	for _, raw := range req.RedirectURIs {
		if !validOAuthRedirectURI(raw) {
			return nil, oauthErr(http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris must be HTTPS, loopback HTTP or a native-app scheme without fragments or credentials")
		}
		if !slices.Contains(uris, raw) {
			uris = append(uris, raw)
		}
	}
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_basic"
	}
	if !slices.Contains([]string{"none", "client_secret_basic", "client_secret_post"}, method) {
		return nil, oauthErr(http.StatusBadRequest, "invalid_client_metadata", "token_endpoint_auth_method must be none, client_secret_basic or client_secret_post")
	}
	for _, grant := range req.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			return nil, oauthErr(http.StatusBadRequest, "invalid_client_metadata", "only authorization_code and refresh_token grants are supported")
		}
	}
	for _, response := range req.ResponseTypes {
		if response != "code" {
			return nil, oauthErr(http.StatusBadRequest, "invalid_client_metadata", "only the code response type is supported")
		}
	}
	now := time.Now()
	if _, err := a.Store.PruneMCPOAuthClients(ctx, now.Add(-oauthProxyClientMaxIdle).Unix()); err != nil {
		return nil, err
	}
	if n, err := a.Store.CountMCPOAuthClients(ctx); err != nil {
		return nil, err
	} else if n >= oauthProxyClientLimit {
		return nil, oauthErr(http.StatusTooManyRequests, "invalid_client_metadata", "too many registered clients; ask the administrator to remove unused registrations")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	client := &domain.MCPOAuthClient{ID: "dcr-" + hex.EncodeToString(id[:]), Name: cleanClientName(req.ClientName), RedirectURIs: uris, AuthMethod: method, CreatedAt: now.Unix()}
	out := &MCPOAuthClientRegistered{ClientID: client.ID, ClientIDIssuedAt: client.CreatedAt, ClientName: client.Name, RedirectURIs: uris, TokenEndpointAuthMethod: method,
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, Scope: strings.Join(a.MCPOAuthConnection().OAuth.ScopesSupported, " ")}
	if method != "none" {
		secret, err := token(32)
		if err != nil {
			return nil, err
		}
		out.ClientSecret = secret
		client.SecretHash = tokenHash(secret)
	}
	if err := a.Store.CreateMCPOAuthClient(ctx, client); err != nil {
		return nil, err
	}
	a.audit(WithActor(ctx, "mcp_oauth_proxy"), "mcp_oauth_client_register", "oauth_client:"+client.ID, "ok", client.Name)
	return out, nil
}

// ListMCPOAuthClients and DeleteMCPOAuthClient let administrators review and
// revoke anonymous registrations; revocation stops new authorizations, and
// refresh tokens sealed for that client stop working on their next use.
func (a *App) ListMCPOAuthClients(ctx context.Context) ([]domain.MCPOAuthClient, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return a.Store.ListMCPOAuthClients(ctx)
}

func (a *App) DeleteMCPOAuthClient(ctx context.Context, id string) error {
	if _, err := requireAdmin(ctx); err != nil {
		return err
	}
	if err := a.Store.DeleteMCPOAuthClient(ctx, id); err != nil {
		return err
	}
	a.audit(ctx, "mcp_oauth_client_delete", "oauth_client:"+id, "ok", "")
	return nil
}

// ---------- authorization ----------

type MCPOAuthAuthorizeRequest struct {
	ResponseType, ClientID, RedirectURI, State, Scope, CodeChallenge, CodeChallengeMethod, Resource string
}

// IsPreregisteredMCPOAuthClient reports whether client_id names one of the
// operator's Keycloak-registered clients, which the proxy passes straight
// through to Keycloak instead of treating as a dynamic registration.
func (a *App) IsPreregisteredMCPOAuthClient(clientID string) bool {
	o, ok := a.MCPOAuthProxy()
	return ok && clientID != "" && clientID != o.Proxy.ClientID && slices.Contains(o.AllowedClientIDs, clientID)
}

// UpstreamMCPOAuthEndpoints returns Keycloak's authorization and token
// endpoints from trusted discovery (never from request data).
func (a *App) UpstreamMCPOAuthEndpoints(ctx context.Context) (oauth2.Endpoint, error) {
	o, ok := a.MCPOAuthProxy()
	if !ok {
		return oauth2.Endpoint{}, domain.ErrNotFound
	}
	p, err := a.mcpProvider(ctx, o.Issuer)
	if err != nil {
		return oauth2.Endpoint{}, err
	}
	endpoint := p.Endpoint()
	if !validOAuthURL(endpoint.AuthURL) || !validOAuthURL(endpoint.TokenURL) {
		return oauth2.Endpoint{}, domain.ErrNotFound
	}
	return endpoint, nil
}

// UpstreamMCPOAuthJWKS returns Keycloak's jwks_uri for authorization-server
// metadata, since the access tokens the proxy hands out are Keycloak's.
func (a *App) UpstreamMCPOAuthJWKS(ctx context.Context) string {
	o, ok := a.MCPOAuthProxy()
	if !ok {
		return ""
	}
	p, err := a.mcpProvider(ctx, o.Issuer)
	if err != nil {
		return ""
	}
	var metadata struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if p.Claims(&metadata) != nil {
		return ""
	}
	return metadata.JWKSURI
}

func validPKCEChallenge(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	return true
}

// resolveProxyScopes narrows a request to the scopes this deployment grants.
// Scopes the policy does not allow are dropped rather than refused, as RFC
// 6749 §3.3 permits, and the token response reports what was granted; a
// request naming nothing usable falls back to the read/search defaults.
func (a *App) resolveProxyScopes(o MCPOAuthInfo, requested string) []string {
	out := []string{}
	for _, scope := range strings.Fields(requested) {
		if slices.Contains(o.ScopesSupported, scope) && !slices.Contains(out, scope) {
			out = append(out, scope)
		}
	}
	if len(out) == 0 {
		for _, scope := range DefaultMCPScopes() {
			if slices.Contains(o.ScopesSupported, scope) {
				out = append(out, scope)
			}
		}
	}
	return out
}

// MCPOAuthConsent is what the consent page shows before Postra forwards a
// dynamically registered client to Keycloak. Because every such client
// shares one upstream Keycloak client, Keycloak cannot tell them apart and an
// existing Keycloak session would otherwise authorize any registrant
// silently (the MCP "confused deputy" problem); the user must see which
// client and redirect target are asking. Request is a sealed, nonce-bound
// copy of the validated request for the confirming POST.
type MCPOAuthConsent struct {
	ClientID    string
	ClientName  string
	RedirectURI string
	Scopes      []string
	Request     string
}

type oauthProxyConsent struct {
	ClientID      string   `json:"c"`
	RedirectURI   string   `json:"r"`
	State         string   `json:"s"`
	CodeChallenge string   `json:"h"`
	Scopes        []string `json:"sc"`
	Nonce         string   `json:"n"`
	ExpiresAt     int64    `json:"e"`
}

// PrepareMCPOAuthProxy validates a client's authorization request and
// returns the consent the browser must confirm. nonce is the transport's
// browser-bound secret (a cookie) that the confirming request must repeat.
func (a *App) PrepareMCPOAuthProxy(ctx context.Context, req MCPOAuthAuthorizeRequest, nonce string) (*MCPOAuthConsent, error) {
	o, ok := a.MCPOAuthProxy()
	if !ok {
		return nil, oauthErr(http.StatusNotFound, "invalid_request", "OAuth proxy is not enabled")
	}
	if req.ClientID == "" || len(req.ClientID) > 128 {
		return nil, oauthErr(http.StatusBadRequest, "invalid_client", "client_id is required")
	}
	client, err := a.Store.GetMCPOAuthClient(ctx, req.ClientID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, oauthErr(http.StatusBadRequest, "invalid_client", "unknown client_id; register the client first")
		}
		return nil, err
	}
	redirectURI := req.RedirectURI
	if redirectURI == "" {
		if len(client.RedirectURIs) != 1 {
			return nil, oauthErr(http.StatusBadRequest, "invalid_request", "redirect_uri is required")
		}
		redirectURI = client.RedirectURIs[0]
	}
	if !slices.ContainsFunc(client.RedirectURIs, func(registered string) bool { return redirectURIMatches(registered, redirectURI) }) {
		return nil, oauthErr(http.StatusBadRequest, "invalid_request", "redirect_uri does not match the registered redirect URIs")
	}
	fail := func(code, description string) error {
		return &MCPOAuthProxyError{Code: code, Description: description, Status: http.StatusFound, RedirectURI: redirectURI, State: req.State}
	}
	if len(req.State) > 2048 {
		return nil, fail("invalid_request", "state is too long")
	}
	if req.ResponseType != "code" {
		return nil, fail("unsupported_response_type", "only response_type=code is supported")
	}
	if req.CodeChallengeMethod != "S256" || !validPKCEChallenge(req.CodeChallenge) {
		return nil, fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
	}
	if req.Resource != "" && req.Resource != o.ResourceURL {
		return nil, fail("invalid_target", "resource must be "+o.ResourceURL)
	}
	scopes := a.resolveProxyScopes(o, req.Scope)
	if len(scopes) == 0 {
		return nil, fail("invalid_scope", "no requested scope is allowed here; supported scopes: "+strings.Join(o.ScopesSupported, " "))
	}
	if nonce == "" {
		return nil, errors.New("consent nonce is required")
	}
	sealed, err := a.sealOAuthProxy("consent", oauthProxyConsent{ClientID: client.ID, RedirectURI: redirectURI, State: req.State, CodeChallenge: req.CodeChallenge, Scopes: scopes, Nonce: nonce, ExpiresAt: time.Now().Add(oauthProxyStateTTL).Unix()})
	if err != nil {
		return nil, err
	}
	return &MCPOAuthConsent{ClientID: client.ID, ClientName: client.Name, RedirectURI: redirectURI, Scopes: scopes, Request: sealed}, nil
}

// BeginMCPOAuthProxy consumes a confirmed consent and returns the Keycloak
// authorization URL, or the client redirect carrying access_denied when the
// user refused. The nonce must match the one sealed at PrepareMCPOAuthProxy.
func (a *App) BeginMCPOAuthProxy(ctx context.Context, sealedRequest, nonce string, allow bool) (string, error) {
	o, ok := a.MCPOAuthProxy()
	if !ok {
		return "", oauthErr(http.StatusNotFound, "invalid_request", "OAuth proxy is not enabled")
	}
	var consent oauthProxyConsent
	if err := a.openOAuthProxy("consent", sealedRequest, &consent); err != nil || time.Now().Unix() > consent.ExpiresAt || nonce == "" || subtle.ConstantTimeCompare([]byte(consent.Nonce), []byte(nonce)) != 1 {
		return "", oauthErr(http.StatusBadRequest, "invalid_request", "the consent request is missing, invalid or expired; start the connection again from the MCP client")
	}
	client, err := a.Store.GetMCPOAuthClient(ctx, consent.ClientID)
	if err != nil {
		return "", oauthErr(http.StatusBadRequest, "invalid_client", "the client registration no longer exists")
	}
	if !allow {
		return MCPOAuthProxyErrorRedirect(&MCPOAuthProxyError{Code: "access_denied", Description: "the user refused the connection", RedirectURI: consent.RedirectURI, State: consent.State}), nil
	}
	endpoint, err := a.UpstreamMCPOAuthEndpoints(ctx)
	if err != nil {
		a.recordIncident(domain.SeverityError, "mcp_oauth", "MCP OAuth 프록시 Keycloak discovery 실패", "Issuer "+o.Issuer)
		return "", &MCPOAuthProxyError{Code: "temporarily_unavailable", Description: "the upstream authorization server could not be discovered", Status: http.StatusFound, RedirectURI: consent.RedirectURI, State: consent.State}
	}
	stateNonce, _ := token(16)
	verifier := oauth2.GenerateVerifier()
	state := oauthProxyState{ClientID: client.ID, RedirectURI: consent.RedirectURI, State: consent.State, CodeChallenge: consent.CodeChallenge, Scopes: consent.Scopes, Verifier: verifier, Nonce: stateNonce, ExpiresAt: time.Now().Add(oauthProxyStateTTL).Unix()}
	sealed, err := a.sealOAuthProxy("state", state)
	if err != nil {
		return "", err
	}
	_ = a.Store.TouchMCPOAuthClient(ctx, client.ID, time.Now().Unix())
	a.audit(WithActor(ctx, "mcp_oauth_proxy"), "mcp_oauth_proxy_consent", "oauth_client:"+client.ID, "ok", consent.RedirectURI)
	cfg := oauth2.Config{ClientID: o.Proxy.ClientID, Endpoint: endpoint, RedirectURL: o.Proxy.CallbackURL, Scopes: append([]string{oidc.ScopeOpenID}, consent.Scopes...)}
	return cfg.AuthCodeURL(sealed, oauth2.S256ChallengeOption(verifier)), nil
}

// CompleteMCPOAuthProxy turns Keycloak's callback into the redirect back to
// the client. The upstream code is exchanged later, at the token endpoint,
// once the client has proven its PKCE verifier.
func (a *App) CompleteMCPOAuthProxy(ctx context.Context, code, sealedState, providerError string) (string, error) {
	if _, ok := a.MCPOAuthProxy(); !ok {
		return "", oauthErr(http.StatusNotFound, "invalid_request", "OAuth proxy is not enabled")
	}
	var state oauthProxyState
	if err := a.openOAuthProxy("state", sealedState, &state); err != nil || time.Now().Unix() > state.ExpiresAt {
		return "", oauthErr(http.StatusBadRequest, "invalid_request", "the login state is missing, invalid or expired; start the connection again")
	}
	target, err := url.Parse(state.RedirectURI)
	if err != nil {
		return "", oauthErr(http.StatusBadRequest, "invalid_request", "invalid redirect target")
	}
	query := target.Query()
	if state.State != "" {
		query.Set("state", state.State)
	}
	if providerError != "" || code == "" {
		// Only the provider's error code (never its free text) travels on.
		outcome := "access_denied"
		if slices.Contains([]string{"access_denied", "login_required", "interaction_required", "consent_required", "invalid_scope", "unauthorized_client", "server_error", "temporarily_unavailable"}, providerError) {
			outcome = providerError
		}
		if outcome != "access_denied" {
			a.recordIncident(domain.SeverityWarning, "mcp_oauth", "MCP OAuth 프록시 Keycloak 인증 거절 ("+outcome+")", "client "+state.ClientID)
		}
		query.Set("error", outcome)
		target.RawQuery = query.Encode()
		return target.String(), nil
	}
	if len(code) > 4096 {
		return "", oauthErr(http.StatusBadRequest, "invalid_request", "invalid authorization code")
	}
	sealed, err := a.sealOAuthProxy("code", oauthProxyCode{ClientID: state.ClientID, RedirectURI: state.RedirectURI, CodeChallenge: state.CodeChallenge, Scopes: state.Scopes,
		UpstreamCode: code, Verifier: state.Verifier, Nonce: state.Nonce, ExpiresAt: time.Now().Add(oauthProxyCodeTTL).Unix()})
	if err != nil {
		return "", err
	}
	query.Set("code", sealed)
	target.RawQuery = query.Encode()
	return target.String(), nil
}

// ---------- token endpoint ----------

type MCPOAuthTokenRequest struct {
	GrantType, Code, RedirectURI, ClientID, ClientSecret, CodeVerifier, RefreshToken string
}

type MCPOAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

func (a *App) oauthProxySecret(ctx context.Context) (string, error) {
	ref := a.Setting(SettingMCPOAuthProxySecretRef)
	if ref == "" {
		return "", nil
	}
	if a.Secrets == nil {
		return "", errors.New("secret store unavailable")
	}
	handle, err := a.Secrets.Acquire(ctx, domain.SecretRef(ref), domain.PurposeOIDC)
	if err != nil {
		return "", err
	}
	secret := string(handle.Reveal())
	handle.Zero()
	return secret, nil
}

// upstreamHTTPContext bounds every call to Keycloak's token endpoint the way
// discovery is bounded: short timeout, no redirects, capped bodies.
func (a *App) upstreamHTTPContext(ctx context.Context, issuer string) context.Context {
	client := &http.Client{Timeout: 15 * time.Second, Transport: &oauthTransport{base: http.DefaultTransport, issuer: issuer, nextRefresh: time.Now().Add(-time.Hour)},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return context.WithValue(ctx, oauth2.HTTPClient, client)
}

func (a *App) authenticateProxyClient(ctx context.Context, req MCPOAuthTokenRequest) (*domain.MCPOAuthClient, error) {
	if req.ClientID == "" || len(req.ClientID) > 128 {
		return nil, oauthErr(http.StatusUnauthorized, "invalid_client", "client_id is required")
	}
	client, err := a.Store.GetMCPOAuthClient(ctx, req.ClientID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, oauthErr(http.StatusUnauthorized, "invalid_client", "unknown client_id")
		}
		return nil, err
	}
	switch client.AuthMethod {
	case "none":
		// Public clients are identified only; a stray secret grants nothing.
	case "client_secret_basic", "client_secret_post":
		// Either transport of the secret is accepted (RFC 6749 §2.3.1); the
		// registered method is advice to the client, not a second factor.
		if req.ClientSecret == "" || subtle.ConstantTimeCompare([]byte(tokenHash(req.ClientSecret)), []byte(client.SecretHash)) != 1 {
			return nil, oauthErr(http.StatusUnauthorized, "invalid_client", "client authentication failed")
		}
	default:
		return nil, oauthErr(http.StatusUnauthorized, "invalid_client", "client authentication failed")
	}
	return client, nil
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// ExchangeMCPOAuthProxy serves authorization_code and refresh_token grants
// for dynamically registered clients. The access token returned is the
// Keycloak token and is verified with the same resource checks as any other
// MCP bearer before it is handed out.
func (a *App) ExchangeMCPOAuthProxy(ctx context.Context, req MCPOAuthTokenRequest) (*MCPOAuthTokenResponse, error) {
	o, ok := a.MCPOAuthProxy()
	if !ok {
		return nil, oauthErr(http.StatusNotFound, "invalid_request", "OAuth proxy is not enabled")
	}
	client, err := a.authenticateProxyClient(ctx, req)
	if err != nil {
		return nil, err
	}
	endpoint, err := a.UpstreamMCPOAuthEndpoints(ctx)
	if err != nil {
		return nil, oauthErr(http.StatusServiceUnavailable, "temporarily_unavailable", "the upstream authorization server could not be discovered")
	}
	secret, err := a.oauthProxySecret(ctx)
	if err != nil {
		slog.Warn("MCP OAuth proxy client secret unavailable", "client_id", o.Proxy.ClientID)
		return nil, oauthErr(http.StatusServiceUnavailable, "temporarily_unavailable", "the proxy client secret is unavailable")
	}
	// A public upstream client identifies itself in the form body; a
	// confidential one authenticates with HTTP Basic, as Keycloak expects.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	if secret != "" {
		endpoint.AuthStyle = oauth2.AuthStyleInHeader
	}
	cfg := oauth2.Config{ClientID: o.Proxy.ClientID, ClientSecret: secret, Endpoint: endpoint, RedirectURL: o.Proxy.CallbackURL}
	upstream := a.upstreamHTTPContext(ctx, o.Issuer)
	var tok *oauth2.Token
	switch req.GrantType {
	case "authorization_code":
		var code oauthProxyCode
		if err := a.openOAuthProxy("code", req.Code, &code); err != nil || code.ClientID != client.ID || time.Now().Unix() > code.ExpiresAt {
			return nil, oauthErr(http.StatusBadRequest, "invalid_grant", "the authorization code is invalid, expired or issued to another client")
		}
		if req.RedirectURI != "" && req.RedirectURI != code.RedirectURI {
			return nil, oauthErr(http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		}
		if !pkceMatches(req.CodeVerifier, code.CodeChallenge) {
			return nil, oauthErr(http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		}
		tok, err = cfg.Exchange(upstream, code.UpstreamCode, oauth2.VerifierOption(code.Verifier))
		if err != nil {
			detail := oidcExchangeGuidance(err)
			slog.Warn("MCP OAuth proxy upstream code exchange failed", "detail", detail, "client_id", o.Proxy.ClientID)
			a.recordIncident(domain.SeverityError, "mcp_oauth", "MCP OAuth 프록시 토큰 교환 실패", detail)
			return nil, oauthErr(http.StatusBadRequest, "invalid_grant", "the upstream authorization server rejected the code: "+detail)
		}
	case "refresh_token":
		var refresh oauthProxyRefresh
		if err := a.openOAuthProxy("refresh", req.RefreshToken, &refresh); err != nil || refresh.ClientID != client.ID || refresh.UpstreamRefresh == "" {
			return nil, oauthErr(http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or issued to another client")
		}
		tok, err = cfg.TokenSource(upstream, &oauth2.Token{RefreshToken: refresh.UpstreamRefresh, Expiry: time.Unix(1, 0)}).Token()
		if err != nil {
			return nil, oauthErr(http.StatusBadRequest, "invalid_grant", "the upstream authorization server rejected the refresh token; sign in again")
		}
	default:
		return nil, oauthErr(http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
	principal, err := a.AuthenticateMCPOAuthToken(ctx, tok.AccessToken)
	if err != nil {
		a.recordIncident(domain.SeverityWarning, "mcp_oauth", "MCP OAuth 프록시가 발급받은 토큰이 MCP 자격 증명으로 거부됨", "client "+o.Proxy.ClientID+": Audience mapper, 허용 Client, 사용자 SSO 연결 확인")
		return nil, oauthErr(http.StatusBadRequest, "invalid_grant", "Keycloak issued a token this MCP resource does not accept: check the audience mapper for "+o.ResourceURL+", the proxy client, and that the user signed in to Postra with SSO first")
	}
	out := &MCPOAuthTokenResponse{AccessToken: tok.AccessToken, TokenType: "Bearer", Scope: strings.Join(principal.MCPScopes, " ")}
	if !tok.Expiry.IsZero() {
		out.ExpiresIn = max(int64(time.Until(tok.Expiry).Seconds()), 1)
	}
	if tok.RefreshToken != "" {
		sealed, err := a.sealOAuthProxy("refresh", oauthProxyRefresh{ClientID: client.ID, UpstreamRefresh: tok.RefreshToken})
		if err != nil {
			return nil, err
		}
		out.RefreshToken = sealed
	}
	_ = a.Store.TouchMCPOAuthClient(ctx, client.ID, time.Now().Unix())
	a.audit(WithPrincipal(WithActor(ctx, "mcp_oauth_proxy"), principal), "mcp_oauth_proxy_token", "oauth_client:"+client.ID, "ok", req.GrantType)
	return out, nil
}

// ForwardMCPOAuthToken relays a token request of an operator pre-registered
// Keycloak client unchanged to Keycloak, so such clients keep working while
// protected-resource metadata points at the proxy. Postra adds nothing: the
// client's own credentials and PKCE verifier authenticate the request.
func (a *App) ForwardMCPOAuthToken(ctx context.Context, form url.Values, authorization string) (int, []byte, error) {
	o, ok := a.MCPOAuthProxy()
	if !ok {
		return 0, nil, oauthErr(http.StatusNotFound, "invalid_request", "OAuth proxy is not enabled")
	}
	endpoint, err := a.UpstreamMCPOAuthEndpoints(ctx)
	if err != nil {
		return 0, nil, oauthErr(http.StatusServiceUnavailable, "temporarily_unavailable", "the upstream authorization server could not be discovered")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	client, _ := a.upstreamHTTPContext(ctx, o.Issuer).Value(oauth2.HTTPClient).(*http.Client)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, oauthErr(http.StatusBadGateway, "temporarily_unavailable", "the upstream authorization server did not answer")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, oauthErr(http.StatusBadGateway, "temporarily_unavailable", "the upstream authorization server did not answer")
	}
	if !json.Valid(body) {
		return 0, nil, oauthErr(http.StatusBadGateway, "temporarily_unavailable", "the upstream authorization server returned a non-JSON answer")
	}
	return resp.StatusCode, body, nil
}

// MCPOAuthProxyErrorRedirect builds the client redirect for an error that may
// safely be reported to the validated redirect URI.
func MCPOAuthProxyErrorRedirect(e *MCPOAuthProxyError) string {
	target, err := url.Parse(e.RedirectURI)
	if err != nil {
		return ""
	}
	query := target.Query()
	query.Set("error", e.Code)
	query.Set("error_description", e.Description)
	if e.State != "" {
		query.Set("state", e.State)
	}
	target.RawQuery = query.Encode()
	return target.String()
}
