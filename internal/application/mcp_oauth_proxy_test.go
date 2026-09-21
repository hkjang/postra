package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"postra/internal/domain"
)

// proxyTestUpstream is a minimal Keycloak token endpoint: it hands out one
// signed access token per authorization code, rejects replayed codes, and
// records what the proxy sent so client-credential and PKCE relaying can be
// asserted without inspecting the wire.
type proxyTestUpstream struct {
	mu       sync.Mutex
	issuer   *oauthTestIssuer
	used     map[string]bool
	requests []url.Values
	auth     []string
	claims   map[string]any
	refresh  string
}

func newProxyTestApp(t *testing.T) (*App, *oauthTestIssuer, *proxyTestUpstream) {
	t.Helper()
	app, issuer := oauthTestApp(t)
	up := &proxyTestUpstream{issuer: issuer, used: map[string]bool{}, refresh: "kc-refresh-1"}
	issuer.tokenEndpoint = func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		up.mu.Lock()
		defer up.mu.Unlock()
		up.requests = append(up.requests, r.PostForm)
		up.auth = append(up.auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			code := r.PostForm.Get("code")
			if up.used[code] || !strings.HasPrefix(code, "kc-code-") {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			up.used[code] = true
		case "refresh_token":
			if r.PostForm.Get("refresh_token") != up.refresh {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			up.refresh += "-next"
		default:
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
			return
		}
		claims := map[string]any{"azp": "postra-mcp-proxy", "scope": "openid mail.read mail.search"}
		for k, v := range up.claims {
			claims[k] = v
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": issuer.token(t, claims), "token_type": "Bearer", "expires_in": 300, "refresh_token": up.refresh, "scope": "openid mail.read mail.search"})
	}
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: map[string]string{SettingMCPOAuthProxyEnabled: "true", SettingMCPOAuthProxyClientID: "postra-mcp-proxy"}}); err != nil {
		t.Fatal(err)
	}
	return app, issuer, up
}

func registerProxyClient(t *testing.T, app *App, req MCPOAuthClientRegistration) *MCPOAuthClientRegistered {
	t.Helper()
	out, err := app.RegisterMCPOAuthClient(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pkcePair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = oauth2.GenerateVerifier()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// consentAndBegin walks the consent step the browser would: prepare with a
// cookie nonce, then confirm with the same nonce.
func consentAndBegin(ctx context.Context, app *App, req MCPOAuthAuthorizeRequest) (string, error) {
	consent, err := app.PrepareMCPOAuthProxy(ctx, req, "cookie-nonce")
	if err != nil {
		return "", err
	}
	return app.BeginMCPOAuthProxy(ctx, consent.Request, "cookie-nonce", true)
}

func proxyErr(t *testing.T, err error) *MCPOAuthProxyError {
	t.Helper()
	var e *MCPOAuthProxyError
	if !errors.As(err, &e) {
		t.Fatalf("expected OAuth error, got %v", err)
	}
	return e
}

func TestMCPOAuthProxySettingsRequireDistinctUpstreamClient(t *testing.T) {
	app, _ := oauthTestApp(t)
	admin := settingsAdmin()
	for name, values := range map[string]map[string]string{
		"without client":      {SettingMCPOAuthProxyEnabled: "true"},
		"web client reused":   {SettingMCPOAuthProxyEnabled: "true", SettingMCPOAuthProxyClientID: "postra-web"},
		"oauth disabled":      {SettingMCPOAuthProxyEnabled: "true", SettingMCPOAuthProxyClientID: "postra-mcp-proxy", "mcp.oauth.enabled": "false"},
		"malformed client id": {SettingMCPOAuthProxyEnabled: "true", SettingMCPOAuthProxyClientID: "bad client"},
	} {
		if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: values}); err == nil {
			t.Fatalf("%s: proxy enabled without a usable upstream client", name)
		}
	}
	if _, ok := app.MCPOAuthProxy(); ok {
		t.Fatal("proxy reported active before being configured")
	}
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{SettingMCPOAuthProxyEnabled: "true", SettingMCPOAuthProxyClientID: "postra-mcp-proxy"}}); err != nil {
		t.Fatal(err)
	}
	o, ok := app.MCPOAuthProxy()
	if !ok || o.AuthorizationServer != "https://postra.test" || o.Proxy.Issuer != "https://postra.test" || o.Proxy.CallbackURL != "https://postra.test/oauth/callback" ||
		o.Proxy.RegistrationEndpoint != "https://postra.test/oauth/register" || !contains(o.AllowedClientIDs, "postra-mcp-proxy") || !contains(o.AllowedClientIDs, "desktop") {
		t.Fatalf("proxy info = %+v", o)
	}
	// The proxy's own client is allowed to present tokens even when not
	// listed explicitly, so a proxy-only deployment needs no extra allow list.
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{"mcp.oauth.allowed_client_ids": ""}}); err != nil {
		t.Fatal(err)
	}
	if o := app.MCPOAuthConnection().OAuth; !o.Configured || len(o.AllowedClientIDs) != 1 || o.AllowedClientIDs[0] != "postra-mcp-proxy" {
		t.Fatalf("proxy-only allow list = %+v", o.AllowedClientIDs)
	}
	if _, err := app.AdminPatchSettings(admin, SettingsPatch{Values: map[string]string{SettingMCPOAuthProxyEnabled: "false"}}); err != nil {
		t.Fatal(err)
	}
	if o := app.MCPOAuthConnection().OAuth; o.AuthorizationServer != o.Issuer {
		t.Fatal("disabled proxy still advertised as the authorization server")
	}
}

func TestMCPOAuthProxyRegistrationValidatesRedirectsAndSecrets(t *testing.T) {
	app, _, _ := newProxyTestApp(t)
	ctx := context.Background()
	for name, uris := range map[string][]string{
		"none":          nil,
		"plain http":    {"http://client.example/cb"},
		"javascript":    {"javascript:alert(1)"},
		"fragment":      {"https://client.example/cb#frag"},
		"credentials":   {"https://user:pw@client.example/cb"},
		"too many":      {"https://a.example/1", "https://a.example/2", "https://a.example/3", "https://a.example/4", "https://a.example/5", "https://a.example/6", "https://a.example/7", "https://a.example/8", "https://a.example/9", "https://a.example/10", "https://a.example/11"},
		"data":          {"data:text/html,hi"},
		"no scheme":     {"client.example/cb"},
		"trailing junk": {"https://client.example/cb "},
	} {
		if _, err := app.RegisterMCPOAuthClient(ctx, MCPOAuthClientRegistration{RedirectURIs: uris}); err == nil {
			t.Fatalf("%s: registration accepted", name)
		}
	}
	if _, err := app.RegisterMCPOAuthClient(ctx, MCPOAuthClientRegistration{RedirectURIs: []string{"https://client.example/cb"}, TokenEndpointAuthMethod: "private_key_jwt"}); err == nil {
		t.Fatal("unsupported auth method accepted")
	}
	if _, err := app.RegisterMCPOAuthClient(ctx, MCPOAuthClientRegistration{RedirectURIs: []string{"https://client.example/cb"}, GrantTypes: []string{"client_credentials"}}); err == nil {
		t.Fatal("client_credentials grant accepted")
	}
	public := registerProxyClient(t, app, MCPOAuthClientRegistration{RedirectURIs: []string{"http://127.0.0.1:51234/callback", "https://claude.ai/api/mcp/auth_callback", "cursor://anysphere.cursor-retrieval/oauth/callback"}, TokenEndpointAuthMethod: "none", ClientName: "Desktop\x00 App"})
	if public.ClientSecret != "" || !strings.HasPrefix(public.ClientID, "dcr-") || public.ClientName != "Desktop App" || len(public.RedirectURIs) != 3 || public.Scope != "mail.read mail.search mail.send" || public.TokenEndpointAuthMethod != "none" {
		t.Fatalf("public registration = %+v", public)
	}
	confidential := registerProxyClient(t, app, MCPOAuthClientRegistration{RedirectURIs: []string{"https://agent.corp.local/oauth/cb"}})
	if confidential.ClientSecret == "" || confidential.TokenEndpointAuthMethod != "client_secret_basic" || confidential.ClientSecretExpiresAt != 0 {
		t.Fatalf("confidential registration = %+v", confidential)
	}
	stored, err := app.Store.GetMCPOAuthClient(ctx, confidential.ClientID)
	if err != nil || stored.SecretHash == "" || stored.SecretHash == confidential.ClientSecret || strings.Contains(stored.SecretHash, confidential.ClientSecret) {
		t.Fatal("client secret stored in the clear or missing")
	}
	clients, err := app.ListMCPOAuthClients(settingsAdmin())
	if err != nil || len(clients) != 2 {
		t.Fatalf("listed %d clients, err %v", len(clients), err)
	}
	if _, err := app.ListMCPOAuthClients(WithPrincipal(ctx, domain.Principal{UserID: "u", Role: domain.RoleUser})); err == nil {
		t.Fatal("non-admin listed OAuth clients")
	}
	if err := app.DeleteMCPOAuthClient(settingsAdmin(), public.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.GetMCPOAuthClient(ctx, public.ClientID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("deleted client still present")
	}
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: map[string]string{SettingMCPOAuthProxyEnabled: "false"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.RegisterMCPOAuthClient(ctx, MCPOAuthClientRegistration{RedirectURIs: []string{"https://client.example/cb"}}); proxyErr(t, err).Status != http.StatusNotFound {
		t.Fatal("registration open while the proxy is disabled")
	}
}

func TestMCPOAuthProxyRegistrationPrunesIdleAndCapsTotal(t *testing.T) {
	app, _, _ := newProxyTestApp(t)
	ctx := context.Background()
	stale := &domain.MCPOAuthClient{ID: "dcr-stale", RedirectURIs: []string{"https://old.example/cb"}, AuthMethod: "none", CreatedAt: time.Now().Add(-40 * 24 * time.Hour).Unix()}
	recent := &domain.MCPOAuthClient{ID: "dcr-recent", RedirectURIs: []string{"https://old.example/cb"}, AuthMethod: "none", CreatedAt: stale.CreatedAt, LastUsedAt: time.Now().Add(-time.Hour).Unix()}
	for _, c := range []*domain.MCPOAuthClient{stale, recent} {
		if err := app.Store.CreateMCPOAuthClient(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	registerProxyClient(t, app, MCPOAuthClientRegistration{RedirectURIs: []string{"https://new.example/cb"}, TokenEndpointAuthMethod: "none"})
	if _, err := app.Store.GetMCPOAuthClient(ctx, "dcr-stale"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("idle registration survived pruning")
	}
	if _, err := app.Store.GetMCPOAuthClient(ctx, "dcr-recent"); err != nil {
		t.Fatal("recently used registration pruned")
	}
	for i := 0; i < oauthProxyClientLimit; i++ {
		if err := app.Store.CreateMCPOAuthClient(ctx, &domain.MCPOAuthClient{ID: fmt.Sprintf("dcr-bulk-%d", i), RedirectURIs: []string{"https://bulk.example/cb"}, AuthMethod: "none", CreatedAt: time.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.RegisterMCPOAuthClient(ctx, MCPOAuthClientRegistration{RedirectURIs: []string{"https://new.example/cb"}}); proxyErr(t, err).Status != http.StatusTooManyRequests {
		t.Fatal("registration cap not enforced")
	}
}

func TestMCPOAuthProxyAuthorizationBindsClientRedirectAndPKCE(t *testing.T) {
	app, issuer, _ := newProxyTestApp(t)
	ctx := context.Background()
	client := registerProxyClient(t, app, MCPOAuthClientRegistration{RedirectURIs: []string{"http://127.0.0.1:3000/cb", "https://client.example/cb"}, TokenEndpointAuthMethod: "none"})
	_, challenge := pkcePair(t)
	good := MCPOAuthAuthorizeRequest{ResponseType: "code", ClientID: client.ClientID, RedirectURI: "https://client.example/cb", State: "client-state", Scope: "mail.read openid", CodeChallenge: challenge, CodeChallengeMethod: "S256", Resource: "https://postra.test/mcp"}

	// Errors before the redirect URI is trusted must not redirect anywhere.
	for name, mutate := range map[string]func(*MCPOAuthAuthorizeRequest){
		"unknown client":    func(r *MCPOAuthAuthorizeRequest) { r.ClientID = "dcr-unknown" },
		"foreign redirect":  func(r *MCPOAuthAuthorizeRequest) { r.RedirectURI = "https://attacker.example/cb" },
		"https port change": func(r *MCPOAuthAuthorizeRequest) { r.RedirectURI = "https://client.example:8443/cb" },
		"missing redirect":  func(r *MCPOAuthAuthorizeRequest) { r.RedirectURI = "" },
	} {
		req := good
		mutate(&req)
		if _, err := app.PrepareMCPOAuthProxy(ctx, req, "n"); proxyErr(t, err).RedirectURI != "" {
			t.Fatalf("%s: error redirected to an untrusted URI", name)
		}
	}
	// Once the redirect URI is trusted, protocol errors go back to the client.
	for name, mutate := range map[string]func(*MCPOAuthAuthorizeRequest){
		"no pkce":        func(r *MCPOAuthAuthorizeRequest) { r.CodeChallenge = "" },
		"plain pkce":     func(r *MCPOAuthAuthorizeRequest) { r.CodeChallengeMethod = "plain" },
		"token response": func(r *MCPOAuthAuthorizeRequest) { r.ResponseType = "token" },
		"other resource": func(r *MCPOAuthAuthorizeRequest) { r.Resource = "https://other.test/mcp" },
	} {
		req := good
		mutate(&req)
		_, err := app.PrepareMCPOAuthProxy(ctx, req, "n")
		e := proxyErr(t, err)
		if e.RedirectURI != "https://client.example/cb" || e.State != "client-state" || !strings.Contains(MCPOAuthProxyErrorRedirect(e), "error="+e.Code) || !strings.Contains(MCPOAuthProxyErrorRedirect(e), "state=client-state") {
			t.Fatalf("%s: error = %+v", name, e)
		}
	}
	consent, err := app.PrepareMCPOAuthProxy(ctx, good, "cookie-nonce")
	if err != nil || consent.ClientID != client.ClientID || consent.RedirectURI != "https://client.example/cb" || strings.Join(consent.Scopes, " ") != "mail.read" || consent.Request == "" {
		t.Fatalf("consent = %+v, err %v", consent, err)
	}
	// Consent is bound to the browser that saw the page and is single-purpose.
	if _, err := app.BeginMCPOAuthProxy(ctx, consent.Request, "other-cookie", true); err == nil {
		t.Fatal("consent confirmed with a different browser nonce")
	}
	if _, err := app.BeginMCPOAuthProxy(ctx, consent.Request, "", true); err == nil {
		t.Fatal("consent confirmed without a browser nonce")
	}
	if app.openOAuthProxy("state", consent.Request, &oauthProxyState{}) == nil {
		t.Fatal("a consent envelope was accepted as upstream state")
	}
	denied, err := app.BeginMCPOAuthProxy(ctx, consent.Request, "cookie-nonce", false)
	if err != nil || !strings.HasPrefix(denied, "https://client.example/cb?") || !strings.Contains(denied, "error=access_denied") || !strings.Contains(denied, "state=client-state") {
		t.Fatalf("denied consent = %s, err %v", denied, err)
	}
	target, err := app.BeginMCPOAuthProxy(ctx, consent.Request, "cookie-nonce", true)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(target)
	if err != nil || !strings.HasPrefix(target, issuer.srv.URL+"/authorize?") {
		t.Fatalf("authorization URL = %s", target)
	}
	q := u.Query()
	if q.Get("client_id") != "postra-mcp-proxy" || q.Get("redirect_uri") != "https://postra.test/oauth/callback" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("code_challenge") == challenge || q.Get("scope") != "openid mail.read" || q.Get("state") == "" || q.Get("state") == "client-state" {
		t.Fatalf("upstream query = %v", q)
	}
	var state oauthProxyState
	if err := app.openOAuthProxy("state", q.Get("state"), &state); err != nil || state.ClientID != client.ClientID || state.CodeChallenge != challenge || state.State != "client-state" || state.Verifier == "" {
		t.Fatalf("sealed state = %+v, err %v", state, err)
	}
	if strings.Contains(target, state.Verifier) || strings.Contains(target, "client-state") {
		t.Fatal("upstream PKCE verifier or client state leaked into the authorization URL")
	}
	if app.openOAuthProxy("code", q.Get("state"), &oauthProxyCode{}) == nil {
		t.Fatal("a state envelope was accepted as an authorization code")
	}
	// Scopes outside the policy are dropped, never escalated; a request with
	// nothing usable falls back to the defaults rather than failing.
	for requested, granted := range map[string]string{"mail.read admin.everything": "mail.read", "mail.delete": "mail.read mail.search", "openid mail.send mail.read": "mail.send mail.read"} {
		narrowed := good
		narrowed.Scope = requested
		consent, err := app.PrepareMCPOAuthProxy(ctx, narrowed, "n")
		if err != nil || strings.Join(consent.Scopes, " ") != granted {
			t.Fatalf("scope %q granted %v, err %v", requested, consent, err)
		}
	}
	// Loopback redirects may change port; default scopes apply when omitted.
	loopback := good
	loopback.RedirectURI, loopback.Scope = "http://127.0.0.1:49152/cb", ""
	target, err = consentAndBegin(ctx, app, loopback)
	if err != nil {
		t.Fatal(err)
	}
	u, _ = url.Parse(target)
	if u.Query().Get("scope") != "openid mail.read mail.search" {
		t.Fatalf("default scope = %q", u.Query().Get("scope"))
	}
	if err := app.openOAuthProxy("state", u.Query().Get("state"), &state); err != nil || state.RedirectURI != "http://127.0.0.1:49152/cb" {
		t.Fatalf("loopback state = %+v", state)
	}
	// Web SSO clients and the proxy's own upstream client are never treated
	// as pass-through clients; operator pre-registered ones are.
	if app.IsPreregisteredMCPOAuthClient("postra-mcp-proxy") || app.IsPreregisteredMCPOAuthClient("postra-web") || app.IsPreregisteredMCPOAuthClient(client.ClientID) || !app.IsPreregisteredMCPOAuthClient("desktop") {
		t.Fatal("pre-registered client detection is wrong")
	}
}

func TestMCPOAuthProxyCallbackAndTokenExchange(t *testing.T) {
	app, issuer, up := newProxyTestApp(t)
	ctx := context.Background()
	client := registerProxyClient(t, app, MCPOAuthClientRegistration{RedirectURIs: []string{"https://client.example/cb"}, TokenEndpointAuthMethod: "none"})
	verifier, challenge := pkcePair(t)
	begin := func(t *testing.T) string {
		t.Helper()
		target, err := consentAndBegin(ctx, app, MCPOAuthAuthorizeRequest{ResponseType: "code", ClientID: client.ClientID, RedirectURI: "https://client.example/cb", State: "s1", CodeChallenge: challenge, CodeChallengeMethod: "S256"})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(target)
		return u.Query().Get("state")
	}
	state := begin(t)

	// Provider errors are relayed by code only, never by description.
	redirect, err := app.CompleteMCPOAuthProxy(ctx, "", state, "access_denied")
	if err != nil || !strings.HasPrefix(redirect, "https://client.example/cb?") || !strings.Contains(redirect, "error=access_denied") || !strings.Contains(redirect, "state=s1") {
		t.Fatalf("provider error redirect = %s, err %v", redirect, err)
	}
	if _, err := app.CompleteMCPOAuthProxy(ctx, "kc-code-1", "tampered"+state, ""); proxyErr(t, err).RedirectURI != "" {
		t.Fatal("tampered state redirected")
	}
	redirect, err = app.CompleteMCPOAuthProxy(ctx, "kc-code-1", state, "")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(redirect)
	code := u.Query().Get("code")
	if u.Scheme+"://"+u.Host+u.Path != "https://client.example/cb" || u.Query().Get("state") != "s1" || code == "" || strings.Contains(redirect, "kc-code-1") {
		t.Fatalf("client redirect = %s", redirect)
	}
	if len(up.requests) != 0 {
		t.Fatal("upstream code exchanged before the client proved PKCE")
	}
	base := MCPOAuthTokenRequest{GrantType: "authorization_code", Code: code, ClientID: client.ClientID, CodeVerifier: verifier, RedirectURI: "https://client.example/cb"}
	for name, mutate := range map[string]func(*MCPOAuthTokenRequest){
		"wrong verifier":   func(r *MCPOAuthTokenRequest) { r.CodeVerifier = oauth2.GenerateVerifier() },
		"other client":     func(r *MCPOAuthTokenRequest) { r.ClientID = "desktop" },
		"other redirect":   func(r *MCPOAuthTokenRequest) { r.RedirectURI = "https://client.example/other" },
		"state as code":    func(r *MCPOAuthTokenRequest) { r.Code = state },
		"unknown grant":    func(r *MCPOAuthTokenRequest) { r.GrantType = "password" },
		"garbage code":     func(r *MCPOAuthTokenRequest) { r.Code = "not-a-code" },
		"missing client":   func(r *MCPOAuthTokenRequest) { r.ClientID = "" },
		"missing verifier": func(r *MCPOAuthTokenRequest) { r.CodeVerifier = "" },
	} {
		req := base
		mutate(&req)
		if _, err := app.ExchangeMCPOAuthProxy(ctx, req); err == nil {
			t.Fatalf("%s: token issued", name)
		}
		if len(up.requests) != 0 {
			t.Fatalf("%s: upstream contacted with an invalid client request", name)
		}
	}
	tok, err := app.ExchangeMCPOAuthProxy(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if tok.TokenType != "Bearer" || tok.AccessToken == "" || tok.RefreshToken == "" || tok.RefreshToken == "kc-refresh-1" || tok.Scope != "mail.read mail.search" || tok.ExpiresIn <= 0 || tok.ExpiresIn > 300 {
		t.Fatalf("token response = %+v", tok)
	}
	if p, err := app.AuthenticateMCPOAuthToken(ctx, tok.AccessToken); err != nil || p.UserID != "oauth-owner" || p.OAuthClientID != "postra-mcp-proxy" || strings.Join(p.MCPScopes, " ") != "mail.read mail.search" {
		t.Fatalf("issued token not accepted by the resource: %+v %v", p, err)
	}
	sent := up.requests[0]
	if sent.Get("client_id") != "postra-mcp-proxy" || sent.Get("code") != "kc-code-1" || sent.Get("redirect_uri") != "https://postra.test/oauth/callback" || sent.Get("code_verifier") == "" || sent.Get("code_verifier") == verifier || sent.Get("client_secret") != "" {
		t.Fatalf("upstream exchange = %v", sent)
	}
	if _, err := app.ExchangeMCPOAuthProxy(ctx, base); proxyErr(t, err).Code != "invalid_grant" {
		t.Fatal("replayed code accepted")
	}

	// Refresh tokens are bound to the client that obtained them.
	if _, err := app.ExchangeMCPOAuthProxy(ctx, MCPOAuthTokenRequest{GrantType: "refresh_token", RefreshToken: "kc-refresh-1", ClientID: client.ClientID}); proxyErr(t, err).Code != "invalid_grant" {
		t.Fatal("raw upstream refresh token accepted")
	}
	other := registerProxyClient(t, app, MCPOAuthClientRegistration{RedirectURIs: []string{"https://other.example/cb"}, TokenEndpointAuthMethod: "none"})
	if _, err := app.ExchangeMCPOAuthProxy(ctx, MCPOAuthTokenRequest{GrantType: "refresh_token", RefreshToken: tok.RefreshToken, ClientID: other.ClientID}); proxyErr(t, err).Code != "invalid_grant" {
		t.Fatal("refresh token accepted for another client")
	}
	refreshed, err := app.ExchangeMCPOAuthProxy(ctx, MCPOAuthTokenRequest{GrantType: "refresh_token", RefreshToken: tok.RefreshToken, ClientID: client.ClientID})
	if err != nil || refreshed.AccessToken == "" || refreshed.RefreshToken == "" || refreshed.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh = %+v, err %v", refreshed, err)
	}
	if last := up.requests[len(up.requests)-1]; last.Get("grant_type") != "refresh_token" || last.Get("refresh_token") != "kc-refresh-1" || last.Get("client_id") != "postra-mcp-proxy" {
		t.Fatalf("upstream refresh = %v", last)
	}
	if _, err := app.ExchangeMCPOAuthProxy(ctx, MCPOAuthTokenRequest{GrantType: "refresh_token", RefreshToken: tok.RefreshToken, ClientID: client.ClientID}); err == nil {
		t.Fatal("rotated refresh token still accepted upstream")
	}

	// Revoking the registration ends refresh access on the next use.
	if err := app.DeleteMCPOAuthClient(settingsAdmin(), client.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ExchangeMCPOAuthProxy(ctx, MCPOAuthTokenRequest{GrantType: "refresh_token", RefreshToken: refreshed.RefreshToken, ClientID: client.ClientID}); proxyErr(t, err).Code != "invalid_client" {
		t.Fatal("deleted client refreshed a token")
	}
	_ = issuer
}

func TestMCPOAuthProxyRejectsUpstreamTokensTheResourceWouldNotAccept(t *testing.T) {
	app, _, up := newProxyTestApp(t)
	ctx := context.Background()
	client := registerProxyClient(t, app, MCPOAuthClientRegistration{RedirectURIs: []string{"https://client.example/cb"}, TokenEndpointAuthMethod: "none"})
	verifier, challenge := pkcePair(t)
	exchange := func(t *testing.T, n string) (*MCPOAuthTokenResponse, error) {
		t.Helper()
		target, err := consentAndBegin(ctx, app, MCPOAuthAuthorizeRequest{ResponseType: "code", ClientID: client.ClientID, RedirectURI: "https://client.example/cb", CodeChallenge: challenge, CodeChallengeMethod: "S256"})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(target)
		redirect, err := app.CompleteMCPOAuthProxy(ctx, "kc-code-"+n, u.Query().Get("state"), "")
		if err != nil {
			t.Fatal(err)
		}
		r, _ := url.Parse(redirect)
		return app.ExchangeMCPOAuthProxy(ctx, MCPOAuthTokenRequest{GrantType: "authorization_code", Code: r.Query().Get("code"), ClientID: client.ClientID, CodeVerifier: verifier})
	}
	// Each refusal must name its own cause: they need different fixes.
	for name, test := range map[string]struct {
		claims map[string]any
		reason string
	}{
		"missing audience": {map[string]any{"aud": "postra-web"}, "audience"},
		"unlinked subject": {map[string]any{"sub": "someone-else"}, "no Postra user is linked"},
		"id token":         {map[string]any{"typ": "ID"}, "not a usable bearer access token"},
	} {
		up.mu.Lock()
		up.claims = test.claims
		up.mu.Unlock()
		tok, err := exchange(t, name)
		e := proxyErr(t, err)
		if tok != nil || e.Code != "invalid_grant" || !strings.Contains(e.Description, test.reason) {
			t.Fatalf("%s: token handed out or unhelpful error: %+v", name, e)
		}
	}
	up.mu.Lock()
	up.claims = nil
	up.mu.Unlock()
	if _, err := exchange(t, "fine"); err != nil {
		t.Fatal(err)
	}
	// Confidential clients must present their secret; the upstream client
	// secret is relayed from the secret store.
	confidential := registerProxyClient(t, app, MCPOAuthClientRegistration{RedirectURIs: []string{"https://agent.example/cb"}})
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Secrets: map[string]string{SettingMCPOAuthProxySecretRef: "upstream-secret-value"}}); err != nil {
		t.Fatal(err)
	}
	target, err := consentAndBegin(ctx, app, MCPOAuthAuthorizeRequest{ResponseType: "code", ClientID: confidential.ClientID, RedirectURI: "https://agent.example/cb", CodeChallenge: challenge, CodeChallengeMethod: "S256"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(target)
	redirect, err := app.CompleteMCPOAuthProxy(ctx, "kc-code-conf", u.Query().Get("state"), "")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := url.Parse(redirect)
	req := MCPOAuthTokenRequest{GrantType: "authorization_code", Code: r.Query().Get("code"), ClientID: confidential.ClientID, CodeVerifier: verifier}
	if _, err := app.ExchangeMCPOAuthProxy(ctx, req); proxyErr(t, err).Code != "invalid_client" {
		t.Fatal("confidential client exchanged without its secret")
	}
	req.ClientSecret = "wrong"
	if _, err := app.ExchangeMCPOAuthProxy(ctx, req); proxyErr(t, err).Code != "invalid_client" {
		t.Fatal("wrong client secret accepted")
	}
	req.ClientSecret = confidential.ClientSecret
	if _, err := app.ExchangeMCPOAuthProxy(ctx, req); err != nil {
		t.Fatal(err)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	last := up.requests[len(up.requests)-1]
	user, password, ok := (&http.Request{Header: http.Header{"Authorization": {up.auth[len(up.auth)-1]}}}).BasicAuth()
	if !ok || user != "postra-mcp-proxy" || password != "upstream-secret-value" {
		t.Fatalf("upstream client secret not relayed with HTTP Basic: %v %q", last, up.auth[len(up.auth)-1])
	}
	if last.Get("client_secret") == confidential.ClientSecret {
		t.Fatal("the dynamic client's secret was forwarded upstream")
	}
}

func TestMCPOAuthProxyEnvelopesAreOpaqueAndKindBound(t *testing.T) {
	app, _, _ := newProxyTestApp(t)
	sealed, err := app.sealOAuthProxy("refresh", oauthProxyRefresh{ClientID: "dcr-x", UpstreamRefresh: "kc-secret-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(sealed)
	if strings.Contains(string(raw), "kc-secret-refresh") || strings.Contains(string(raw), "dcr-x") {
		t.Fatal("envelope leaks its contents")
	}
	var refresh oauthProxyRefresh
	if err := app.openOAuthProxy("code", sealed, &refresh); err == nil {
		t.Fatal("refresh envelope opened as a code")
	}
	if err := app.openOAuthProxy("refresh", sealed[:len(sealed)-2]+"zz", &refresh); err == nil {
		t.Fatal("tampered envelope opened")
	}
	if err := app.openOAuthProxy("refresh", sealed, &refresh); err != nil || refresh.UpstreamRefresh != "kc-secret-refresh" {
		t.Fatal("envelope round trip failed")
	}
	// Another process sharing the same stored state key (a second replica)
	// opens the envelope; a process with a different key does not.
	sibling, _ := oauthTestApp(t)
	if err := sibling.openOAuthProxy("refresh", sealed, &refresh); err == nil {
		t.Fatal("envelope opened under a different key")
	}
	sibling.oidcStateKey = app.oidcStateKey
	if err := sibling.openOAuthProxy("refresh", sealed, &refresh); err != nil {
		t.Fatal("replica sharing the state key could not open the envelope")
	}
}
