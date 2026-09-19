package mcpserver

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"postra/internal/application"
)

// IsOAuthProxyPath reports whether a request belongs to the DCR-compatible
// authorization-server proxy, which shares the MCP listener so the issuer
// (the MCP public origin) resolves to the same process.
func IsOAuthProxyPath(path string) bool {
	switch path {
	case application.OAuthProxyMetadataPath, application.OAuthProxyOpenIDPath, application.OAuthProxyRegisterPath,
		application.OAuthProxyAuthorizePath, application.OAuthProxyCallbackPath, application.OAuthProxyTokenPath:
		return true
	}
	return false
}

// ipLimiter is a fixed-window per-address limiter for the anonymous OAuth
// endpoints. It is per process; Keycloak and the sealed envelopes remain the
// real authority, this only blunts registration floods and code guessing.
type ipLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*ipBucket
}

type ipBucket struct {
	start time.Time
	count int
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{limit: limit, window: window, buckets: map[string]*ipBucket{}}
}

func (l *ipLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 10000 {
		for key, b := range l.buckets {
			if now.Sub(b.start) > l.window {
				delete(l.buckets, key)
			}
		}
	}
	b := l.buckets[ip]
	if b == nil || now.Sub(b.start) > l.window {
		l.buckets[ip] = &ipBucket{start: now, count: 1}
		return true
	}
	b.count++
	return b.count <= l.limit
}

type oauthProxy struct {
	app         *application.App
	register    *ipLimiter
	browse      *ipLimiter
	token       *ipLimiter
	crossOrigin *http.CrossOriginProtection
}

// OAuthProxyHandler serves RFC 8414 metadata, RFC 7591 registration and the
// authorization, callback and token endpoints of the proxy. Every path is 404
// until an administrator enables the proxy, and no URL is ever derived from
// request headers.
func OAuthProxyHandler(app *application.App) http.Handler {
	// Limits are per source address; behind a NAT or reverse proxy that is a
	// whole site, so they only stop floods, never ordinary login bursts.
	p := &oauthProxy{app: app, register: newIPLimiter(60, time.Minute), browse: newIPLimiter(300, time.Minute), token: newIPLimiter(600, time.Minute), crossOrigin: http.NewCrossOriginProtection()}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if _, ok := app.MCPOAuthProxy(); !ok {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case application.OAuthProxyMetadataPath, application.OAuthProxyOpenIDPath:
			p.metadata(w, r)
		case application.OAuthProxyRegisterPath:
			p.registerClient(w, r)
		case application.OAuthProxyAuthorizePath:
			p.authorize(w, r)
		case application.OAuthProxyCallbackPath:
			p.callback(w, r)
		case application.OAuthProxyTokenPath:
			p.tokenEndpoint(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func writeOAuthJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOAuthError(w http.ResponseWriter, err error) {
	var oauthErr *application.MCPOAuthProxyError
	if !errors.As(err, &oauthErr) {
		writeOAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error", "error_description": "internal error"})
		return
	}
	status := oauthErr.Status
	if status == 0 || status == http.StatusFound {
		status = http.StatusBadRequest
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="postra-mcp-oauth"`)
	}
	writeOAuthJSON(w, status, map[string]string{"error": oauthErr.Code, "error_description": oauthErr.Description})
}

// writeOAuthPage renders an error the browser must see itself, because the
// client or its redirect URI could not be trusted.
func writeOAuthPage(w http.ResponseWriter, err error) {
	code, description, status := "server_error", "internal error", http.StatusInternalServerError
	var oauthErr *application.MCPOAuthProxyError
	if errors.As(err, &oauthErr) {
		code, description, status = oauthErr.Code, oauthErr.Description, oauthErr.Status
		if status == 0 || status == http.StatusFound {
			status = http.StatusBadRequest
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<!doctype html><html lang=\"ko\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>Postra MCP OAuth</title><style>body{font-family:system-ui,sans-serif;margin:3rem auto;max-width:36rem;padding:0 1rem;color:#1f2933}code{background:#f1f5f9;padding:.1rem .3rem}</style></head><body><h1>MCP 연결을 계속할 수 없습니다</h1><p>오류: <code>"+
		html.EscapeString(code)+"</code></p><p>"+html.EscapeString(description)+"</p><p>MCP 클라이언트에서 연결을 다시 시작하세요. 문제가 계속되면 관리자에게 이 오류 코드를 알려 주세요.</p></body></html>")
}

func (p *oauthProxy) metadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "mcp-protocol-version")
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	o, _ := p.app.MCPOAuthProxy()
	doc := map[string]any{
		"issuer":                                o.Proxy.Issuer,
		"authorization_endpoint":                o.Proxy.AuthorizationEndpoint,
		"token_endpoint":                        o.Proxy.TokenEndpoint,
		"registration_endpoint":                 o.Proxy.RegistrationEndpoint,
		"scopes_supported":                      o.ScopesSupported,
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic", "client_secret_post"},
		"code_challenge_methods_supported":      []string{"S256"},
		"service_documentation":                 "https://github.com/hkjang/postra/blob/main/docs/MCP_OAUTH.md",
	}
	if jwks := p.app.UpstreamMCPOAuthJWKS(r.Context()); jwks != "" {
		doc["jwks_uri"] = jwks
	}
	writeOAuthJSON(w, http.StatusOK, doc)
}

func (p *oauthProxy) registerClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOAuthJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request", "error_description": "POST a JSON client metadata document"})
		return
	}
	if !p.register.allow(application.ClientIP(r.RemoteAddr)) {
		writeOAuthJSON(w, http.StatusTooManyRequests, map[string]string{"error": "invalid_request", "error_description": "too many registrations; retry later"})
		return
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata", "error_description": "Content-Type must be application/json"})
		return
	}
	var req application.MCPOAuthClientRegistration
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata", "error_description": "the client metadata document is not valid JSON"})
		return
	}
	out, err := p.app.RegisterMCPOAuthClient(r.Context(), req)
	if err != nil {
		writeOAuthError(w, err)
		return
	}
	writeOAuthJSON(w, http.StatusCreated, out)
}

const oauthConsentCookie = "postra_oauth_consent"

var oauthScopeLabels = map[string]string{
	"mail.read": "메일 읽기", "mail.search": "메일 검색", "mail.ai": "AI 분석", "mail.draft": "초안·서식·서명", "mail.send": "승인된 메일 발송",
	"mail.delete": "메일 삭제", "mail.work": "분류·규칙·업무", "admin.read": "관리 정보 읽기", "admin.write": "관리 설정 변경",
}

// authorize shows the consent page on GET and, on a same-site POST that
// repeats the consent cookie, forwards the browser to Keycloak. Operator
// pre-registered clients skip consent: Keycloak knows and verifies them.
func (p *oauthProxy) authorize(w http.ResponseWriter, r *http.Request) {
	if !p.browse.allow(application.ClientIP(r.RemoteAddr)) {
		writeOAuthPage(w, &application.MCPOAuthProxyError{Code: "temporarily_unavailable", Description: "too many authorization requests; retry later", Status: http.StatusTooManyRequests})
		return
	}
	switch r.Method {
	case http.MethodGet:
		p.authorizeStart(w, r)
	case http.MethodPost:
		p.crossOrigin.Handler(http.HandlerFunc(p.authorizeConfirm)).ServeHTTP(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *oauthProxy) authorizeStart(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	clientID := params.Get("client_id")
	if p.app.IsPreregisteredMCPOAuthClient(clientID) {
		endpoint, err := p.app.UpstreamMCPOAuthEndpoints(r.Context())
		if err != nil {
			writeOAuthPage(w, &application.MCPOAuthProxyError{Code: "temporarily_unavailable", Description: "the upstream authorization server could not be discovered", Status: http.StatusServiceUnavailable})
			return
		}
		http.Redirect(w, r, endpoint.AuthURL+"?"+params.Encode(), http.StatusFound) // #nosec G710 -- trusted discovery URL; client parameters travel only in the query
		return
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		writeOAuthPage(w, err)
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(entropy[:])
	consent, err := p.app.PrepareMCPOAuthProxy(r.Context(), application.MCPOAuthAuthorizeRequest{
		ResponseType: params.Get("response_type"), ClientID: clientID, RedirectURI: params.Get("redirect_uri"), State: params.Get("state"),
		Scope: params.Get("scope"), CodeChallenge: params.Get("code_challenge"), CodeChallengeMethod: params.Get("code_challenge_method"), Resource: params.Get("resource")}, nonce)
	if err != nil {
		p.authorizeError(w, r, err)
		return
	}
	// SameSite=Lax: sent on our own top-level navigations and the same-site
	// form POST, never on a cross-site POST, so a page elsewhere cannot
	// submit consent on the user's behalf.
	o, _ := p.app.MCPOAuthProxy()
	http.SetCookie(w, &http.Cookie{Name: oauthConsentCookie, Value: nonce, Path: application.OAuthProxyAuthorizePath, MaxAge: 600, HttpOnly: true, Secure: strings.HasPrefix(o.Proxy.Issuer, "https://"), SameSite: http.SameSiteLaxMode}) // #nosec G124 -- one-shot consent binding, no session; Secure follows the configured public origin
	p.consentPage(w, consent)
}

func (p *oauthProxy) authorizeConfirm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthPage(w, &application.MCPOAuthProxyError{Code: "invalid_request", Description: "malformed form body", Status: http.StatusBadRequest})
		return
	}
	nonce := ""
	if cookie, err := r.Cookie(oauthConsentCookie); err == nil {
		nonce = cookie.Value
	}
	http.SetCookie(w, &http.Cookie{Name: oauthConsentCookie, Value: "", Path: application.OAuthProxyAuthorizePath, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode}) // #nosec G124 -- clearing
	target, err := p.app.BeginMCPOAuthProxy(r.Context(), r.PostForm.Get("request"), nonce, r.PostForm.Get("decision") == "allow")
	if err != nil {
		p.authorizeError(w, r, err)
		return
	}
	http.Redirect(w, r, target, http.StatusFound) // #nosec G710 -- Keycloak URL from trusted discovery, or the client's registered redirect URI
}

func (p *oauthProxy) authorizeError(w http.ResponseWriter, r *http.Request, err error) {
	var oauthErr *application.MCPOAuthProxyError
	if errors.As(err, &oauthErr) && oauthErr.RedirectURI != "" {
		if redirect := application.MCPOAuthProxyErrorRedirect(oauthErr); redirect != "" {
			http.Redirect(w, r, redirect, http.StatusFound) // #nosec G710 -- redirect URI was validated against the client's registration
			return
		}
	}
	writeOAuthPage(w, err)
}

func (p *oauthProxy) consentPage(w http.ResponseWriter, c *application.MCPOAuthConsent) {
	name := c.ClientName
	if name == "" {
		name = "(이름 없는 클라이언트)"
	}
	var scopes strings.Builder
	for _, scope := range c.Scopes {
		label := oauthScopeLabels[scope]
		if label == "" {
			label = scope
		}
		scopes.WriteString("<li>" + html.EscapeString(label) + " <code>" + html.EscapeString(scope) + "</code></li>")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<!doctype html><html lang=\"ko\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>Postra MCP 연결 확인</title><style>body{font-family:system-ui,sans-serif;margin:3rem auto;max-width:36rem;padding:0 1rem;color:#1f2933;line-height:1.5}code{background:#f1f5f9;padding:.1rem .3rem;overflow-wrap:anywhere}dl{display:grid;grid-template-columns:auto 1fr;gap:.4rem 1rem}dt{font-weight:600}dd{margin:0;min-width:0}ul{margin:0;padding-left:1.2rem}form{display:flex;gap:.75rem;margin-top:1.5rem}button{font:inherit;padding:.6rem 1.2rem;border-radius:.4rem;border:1px solid #94a3b8;background:#fff;cursor:pointer}button.allow{background:#1d4ed8;border-color:#1d4ed8;color:#fff}.warn{background:#fff7ed;border:1px solid #fdba74;padding:.75rem;border-radius:.4rem}</style></head><body>"+
		"<h1>이 MCP 클라이언트를 내 메일에 연결할까요?</h1><p>아래 클라이언트가 Postra MCP 사용 권한을 요청했습니다. 허용하면 Keycloak 로그인으로 이동하며, 발급된 토큰은 이 클라이언트의 콜백 주소로 전달됩니다.</p>"+
		"<dl><dt>클라이언트</dt><dd>"+html.EscapeString(name)+"<br><code>"+html.EscapeString(c.ClientID)+"</code></dd><dt>콜백 주소</dt><dd><code>"+html.EscapeString(c.RedirectURI)+"</code></dd><dt>요청 권한</dt><dd><ul>"+scopes.String()+"</ul></dd></dl>"+
		"<p class=\"warn\">직접 시작한 연결이 아니거나 콜백 주소를 모르겠다면 거부하세요. 이 클라이언트는 스스로 등록한 것이며 Postra나 Keycloak이 신원을 보증하지 않습니다.</p>"+
		"<form method=\"post\" action=\""+html.EscapeString(application.OAuthProxyAuthorizePath)+"\"><input type=\"hidden\" name=\"request\" value=\""+html.EscapeString(c.Request)+"\"><button type=\"submit\" name=\"decision\" value=\"deny\">거부</button><button class=\"allow\" type=\"submit\" name=\"decision\" value=\"allow\">허용하고 로그인</button></form></body></html>")
}

func (p *oauthProxy) callback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.browse.allow(application.ClientIP(r.RemoteAddr)) {
		writeOAuthPage(w, &application.MCPOAuthProxyError{Code: "temporarily_unavailable", Description: "too many requests; retry later", Status: http.StatusTooManyRequests})
		return
	}
	q := r.URL.Query()
	target, err := p.app.CompleteMCPOAuthProxy(r.Context(), q.Get("code"), q.Get("state"), q.Get("error"))
	if err != nil {
		writeOAuthPage(w, err)
		return
	}
	http.Redirect(w, r, target, http.StatusFound) // #nosec G710 -- redirect URI comes from the sealed state written at /oauth/authorize
}

func (p *oauthProxy) tokenEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOAuthJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request", "error_description": "POST an application/x-www-form-urlencoded token request"})
		return
	}
	if !p.token.allow(application.ClientIP(r.RemoteAddr)) {
		writeOAuthJSON(w, http.StatusTooManyRequests, map[string]string{"error": "invalid_request", "error_description": "too many token requests; retry later"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "malformed form body"})
		return
	}
	form := url.Values{}
	for key, values := range r.PostForm {
		if len(values) > 0 {
			form.Set(key, values[0])
		}
	}
	clientID, clientSecret := form.Get("client_id"), form.Get("client_secret")
	if user, password, ok := r.BasicAuth(); ok {
		if clientID != "" && clientID != user {
			writeOAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "client_id differs between the Authorization header and the body"})
			return
		}
		clientID, clientSecret = user, password
	}
	if p.app.IsPreregisteredMCPOAuthClient(clientID) {
		status, body, err := p.app.ForwardMCPOAuthToken(r.Context(), form, r.Header.Get("Authorization"))
		if err != nil {
			writeOAuthError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body) // #nosec G705 -- json.Valid-checked upstream token response served as application/json with nosniff, never HTML
		return
	}
	out, err := p.app.ExchangeMCPOAuthProxy(r.Context(), application.MCPOAuthTokenRequest{
		GrantType: form.Get("grant_type"), Code: form.Get("code"), RedirectURI: form.Get("redirect_uri"), ClientID: clientID, ClientSecret: clientSecret,
		CodeVerifier: form.Get("code_verifier"), RefreshToken: form.Get("refresh_token")})
	if err != nil {
		writeOAuthError(w, err)
		return
	}
	w.Header().Set("Pragma", "no-cache")
	writeOAuthJSON(w, http.StatusOK, out)
}
