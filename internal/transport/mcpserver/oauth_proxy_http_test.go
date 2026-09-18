package mcpserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"postra/internal/application"
	"postra/internal/domain"
)

type proxyFixture struct {
	*mcpOAuthFixture
	mu       sync.Mutex
	used     map[string]bool
	upstream []url.Values
}

func newProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()
	f := &proxyFixture{mcpOAuthFixture: newMCPOAuthFixture(t), used: map[string]bool{}}
	f.tokenEndpoint = func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.upstream = append(f.upstream, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		code := r.PostForm.Get("code")
		if r.PostForm.Get("grant_type") == "authorization_code" && (f.used[code] || !strings.HasPrefix(code, "kc-code-")) || r.PostForm.Get("grant_type") == "refresh_token" && r.PostForm.Get("refresh_token") != "kc-refresh" {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "upstream says no"})
			return
		}
		f.used[code] = true
		azp := "postra-mcp-proxy"
		if r.PostForm.Get("client_id") == "desktop" {
			azp = "desktop"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.token(t, map[string]any{"azp": azp}), "token_type": "Bearer", "expires_in": 300, "refresh_token": "kc-refresh"})
	}
	f.patch(t, map[string]string{application.SettingMCPOAuthProxyEnabled: "true", application.SettingMCPOAuthProxyClientID: "postra-mcp-proxy"})
	return f
}

func proxyRequest(handler http.Handler, method, target string, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestMCPOAuthProxyIsInvisibleUntilEnabled(t *testing.T) {
	f := newMCPOAuthFixture(t)
	for _, handler := range []http.Handler{OAuthProxyHandler(f.app), HTTPHandler(f.app, "")} {
		for _, path := range []string{application.OAuthProxyMetadataPath, application.OAuthProxyOpenIDPath, application.OAuthProxyRegisterPath, application.OAuthProxyAuthorizePath, application.OAuthProxyCallbackPath, application.OAuthProxyTokenPath} {
			for _, method := range []string{"GET", "POST"} {
				if rec := proxyRequest(handler, method, "https://postra.test"+path, "", nil); rec.Code != 404 {
					t.Fatalf("%s %s served while the proxy is disabled: %d", method, path, rec.Code)
				}
			}
		}
	}
	rec := proxyRequest(OAuthMetadataHandler(f.app), "GET", "https://postra.test"+OAuthMetadataPath+"/mcp", "", nil)
	if !strings.Contains(rec.Body.String(), `"authorization_servers":["`+f.issuer.URL+`"]`) {
		t.Fatalf("resource metadata should point at Keycloak: %s", rec.Body.String())
	}
}

func TestMCPOAuthProxyMetadataAndResourceDiscovery(t *testing.T) {
	f := newProxyFixture(t)
	for _, handler := range []http.Handler{OAuthProxyHandler(f.app), HTTPHandler(f.app, "")} {
		for _, path := range []string{application.OAuthProxyMetadataPath, application.OAuthProxyOpenIDPath} {
			rec := proxyRequest(handler, "GET", "https://attacker.test"+path, "", map[string]string{"Forwarded": "host=attacker.test", "X-Forwarded-Host": "attacker.test"})
			var doc struct {
				Issuer, Authorization, Token, Registration, JWKS string
				Challenge                                        []string `json:"code_challenge_methods_supported"`
				Scopes                                           []string `json:"scopes_supported"`
				Methods                                          []string `json:"token_endpoint_auth_methods_supported"`
			}
			raw := rec.Body.Bytes()
			var m map[string]any
			if rec.Code != 200 || json.Unmarshal(raw, &m) != nil || json.Unmarshal(raw, &doc) != nil {
				t.Fatalf("metadata %s: %d %s", path, rec.Code, raw)
			}
			if m["issuer"] != "https://postra.test" || m["authorization_endpoint"] != "https://postra.test/oauth/authorize" || m["token_endpoint"] != "https://postra.test/oauth/token" ||
				m["registration_endpoint"] != "https://postra.test/oauth/register" || m["jwks_uri"] != f.issuer.URL+"/keys" || len(doc.Challenge) != 1 || doc.Challenge[0] != "S256" ||
				strings.Join(doc.Scopes, " ") != "mail.read mail.search mail.draft mail.send" || len(doc.Methods) != 3 || strings.Contains(string(raw), "attacker.test") {
				t.Fatalf("metadata %s = %s", path, raw)
			}
			if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("metadata must be readable cross-origin and never cached")
			}
		}
	}
	rec := proxyRequest(OAuthMetadataHandler(f.app), "GET", "https://postra.test"+OAuthMetadataPath+"/mcp", "", nil)
	if !strings.Contains(rec.Body.String(), `"authorization_servers":["https://postra.test"]`) {
		t.Fatalf("resource metadata should point at the proxy: %s", rec.Body.String())
	}
	if rec := proxyRequest(OAuthProxyHandler(f.app), "OPTIONS", "https://postra.test"+application.OAuthProxyMetadataPath, "", nil); rec.Code != 204 {
		t.Fatal("CORS preflight on metadata refused")
	}
	if rec := proxyRequest(OAuthProxyHandler(f.app), "PUT", "https://postra.test"+application.OAuthProxyMetadataPath, "", nil); rec.Code != 405 {
		t.Fatal("unexpected method accepted on metadata")
	}
}

func TestMCPOAuthProxyEndToEndOverHTTP(t *testing.T) {
	f := newProxyFixture(t)
	handler := HTTPHandler(f.app, "")

	// 1. Dynamic registration.
	rec := proxyRequest(handler, "POST", "https://postra.test/oauth/register", `{"redirect_uris":["http://127.0.0.1:41234/callback"],"token_endpoint_auth_method":"none","client_name":"Test Desktop","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`, map[string]string{"Content-Type": "application/json"})
	var registered struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &registered) != nil || registered.ClientID == "" || registered.ClientSecret != "" {
		t.Fatalf("registration: %d %s", rec.Code, rec.Body.String())
	}
	if rec := proxyRequest(handler, "POST", "https://postra.test/oauth/register", `{"redirect_uris":["https://x.example/cb"]}`, map[string]string{"Content-Type": "text/plain"}); rec.Code != 400 {
		t.Fatal("registration accepted a non-JSON content type")
	}
	if rec := proxyRequest(handler, "GET", "https://postra.test/oauth/register", "", nil); rec.Code != 405 {
		t.Fatal("registration accepted GET")
	}

	// 2. Authorization redirects the browser to Keycloak with the proxy client.
	verifier := oauth2.GenerateVerifier()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authorize := "https://postra.test/oauth/authorize?" + url.Values{"response_type": {"code"}, "client_id": {registered.ClientID}, "redirect_uri": {"http://127.0.0.1:53000/callback"},
		"state": {"abc"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"mail.read mail.search"}, "resource": {"https://postra.test/mcp"}}.Encode()
	rec = proxyRequest(handler, "GET", authorize, "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "Test Desktop") || !strings.Contains(rec.Body.String(), "http://127.0.0.1:53000/callback") ||
		!strings.Contains(rec.Body.String(), "mail.read") || strings.Contains(rec.Body.String(), "<script") || rec.Header().Get("Location") != "" {
		t.Fatalf("consent page: %d %s", rec.Code, rec.Body.String())
	}
	consentCookie := rec.Result().Cookies()
	if len(consentCookie) != 1 || consentCookie[0].Name != oauthConsentCookie || !consentCookie[0].HttpOnly || !consentCookie[0].Secure || consentCookie[0].SameSite != http.SameSiteLaxMode || consentCookie[0].Path != "/oauth/authorize" {
		t.Fatalf("consent cookie = %+v", consentCookie)
	}
	request := regexp.MustCompile(`name="request" value="([^"]+)"`).FindStringSubmatch(rec.Body.String())
	if request == nil {
		t.Fatal("consent page carries no sealed request")
	}
	confirm := url.Values{"request": {request[1]}, "decision": {"allow"}}.Encode()
	formHeaders := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": oauthConsentCookie + "=" + consentCookie[0].Value}
	// A cross-site or cookie-less confirmation never reaches Keycloak.
	if rec := proxyRequest(handler, "POST", "https://postra.test/oauth/authorize", confirm, map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": formHeaders["Cookie"], "Sec-Fetch-Site": "cross-site"}); rec.Code != 403 {
		t.Fatalf("cross-site consent: %d", rec.Code)
	}
	if rec := proxyRequest(handler, "POST", "https://postra.test/oauth/authorize", confirm, map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": formHeaders["Cookie"], "Origin": "https://attacker.test"}); rec.Code != 403 {
		t.Fatalf("foreign-origin consent: %d", rec.Code)
	}
	if rec := proxyRequest(handler, "POST", "https://postra.test/oauth/authorize", confirm, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); rec.Code != 400 || rec.Header().Get("Location") != "" {
		t.Fatalf("cookie-less consent: %d", rec.Code)
	}
	if rec := proxyRequest(handler, "POST", "https://postra.test/oauth/authorize", url.Values{"request": {request[1]}, "decision": {"deny"}}.Encode(), formHeaders); rec.Code != 302 || !strings.HasPrefix(rec.Header().Get("Location"), "http://127.0.0.1:53000/callback?") || !strings.Contains(rec.Header().Get("Location"), "error=access_denied") {
		t.Fatalf("denied consent: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = proxyRequest(handler, "POST", "https://postra.test/oauth/authorize", confirm, formHeaders)
	location, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != 302 || err != nil || !strings.HasPrefix(location.String(), f.issuer.URL+"/authorize?") || location.Query().Get("client_id") != "postra-mcp-proxy" || location.Query().Get("redirect_uri") != "https://postra.test/oauth/callback" {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	upstreamState := location.Query().Get("state")

	// Untrusted clients get a page, not a redirect; trusted ones a redirect with the error.
	rec = proxyRequest(handler, "GET", strings.Replace(authorize, registered.ClientID, "dcr-nobody", 1), "", nil)
	if rec.Code != 400 || rec.Header().Get("Location") != "" || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || strings.Contains(rec.Body.String(), "<script") || !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("unknown client: %d %s", rec.Code, rec.Body.String())
	}
	rec = proxyRequest(handler, "GET", strings.Replace(authorize, "S256", "plain", 1), "", nil)
	if rec.Code != 302 || !strings.HasPrefix(rec.Header().Get("Location"), "http://127.0.0.1:53000/callback?") || !strings.Contains(rec.Header().Get("Location"), "error=invalid_request") || !strings.Contains(rec.Header().Get("Location"), "state=abc") {
		t.Fatalf("plain PKCE: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// 3. Keycloak calls back; the browser is sent to the client with a sealed code.
	rec = proxyRequest(handler, "GET", "https://postra.test/oauth/callback?code=kc-code-http&state="+url.QueryEscape(upstreamState), "", nil)
	client, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != 302 || err != nil || client.Host != "127.0.0.1:53000" || client.Query().Get("state") != "abc" || client.Query().Get("code") == "" || client.Query().Get("code") == "kc-code-http" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := proxyRequest(handler, "GET", "https://postra.test/oauth/callback?code=kc-code-http&state=forged", "", nil); rec.Code != 400 || rec.Header().Get("Location") != "" {
		t.Fatal("forged state redirected")
	}

	// 4. Token exchange: PKCE is checked before Keycloak is contacted.
	form := url.Values{"grant_type": {"authorization_code"}, "code": {client.Query().Get("code")}, "client_id": {registered.ClientID}, "code_verifier": {oauth2.GenerateVerifier()}, "redirect_uri": {"http://127.0.0.1:53000/callback"}}
	rec = proxyRequest(handler, "POST", "https://postra.test/oauth/token", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"error":"invalid_grant"`) || len(f.upstream) != 0 {
		t.Fatalf("wrong verifier: %d %s (upstream calls %d)", rec.Code, rec.Body.String(), len(f.upstream))
	}
	form.Set("code_verifier", verifier)
	rec = proxyRequest(handler, "POST", "https://postra.test/oauth/token", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	var tok struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &tok) != nil || tok.TokenType != "Bearer" || tok.AccessToken == "" || tok.RefreshToken == "" || tok.RefreshToken == "kc-refresh" || tok.Scope != "mail.read mail.search" || tok.ExpiresIn <= 0 {
		t.Fatalf("token: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || strings.Contains(rec.Body.String(), "id_token") {
		t.Fatal("token response must not be cacheable or carry an ID token")
	}

	// 5. The access token works against MCP as the linked user with the
	// granted scopes, and refresh rotates the sealed refresh token.
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	session, _ := convergenceClient(t, server.URL, tok.AccessToken)
	p := mcpDecode[domain.Principal](t, mcpCall(t, session, "mail_identity", map[string]any{}))
	if p.UserID != application.DefaultUserID || p.AuthMethod != "mcp_oauth" {
		t.Fatalf("identity through the proxy token = %+v", p)
	}
	if result := mcpCall(t, session, "mail_draft_create", map[string]any{"account_id": "acc_mcp", "to": []string{"a@example.test"}, "subject": "x", "body": "y"}); !result.IsError {
		t.Fatal("draft created with only read/search scopes")
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {registered.ClientID}}
	rec = proxyRequest(handler, "POST", "https://postra.test/oauth/token", refresh.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"refresh_token":"`) || strings.Contains(rec.Body.String(), `"refresh_token":"kc-refresh"`) {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	refresh.Set("refresh_token", "kc-refresh")
	rec = proxyRequest(handler, "POST", "https://postra.test/oauth/token", refresh.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatal("raw upstream refresh token accepted over HTTP")
	}
	if rec := proxyRequest(handler, "POST", "https://postra.test/oauth/token", "grant_type=authorization_code", map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("dcr-nobody:secret"))}); rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("unknown Basic client: %d", rec.Code)
	}
}

func TestMCPOAuthProxyPassesPreregisteredClientsThrough(t *testing.T) {
	f := newProxyFixture(t)
	handler := HTTPHandler(f.app, "")
	query := url.Values{"response_type": {"code"}, "client_id": {"desktop"}, "redirect_uri": {"http://127.0.0.1:7000/cb"}, "state": {"s"}, "code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
	rec := proxyRequest(handler, "GET", "https://postra.test/oauth/authorize?"+query.Encode(), "", nil)
	if rec.Code != 302 || rec.Header().Get("Location") != f.issuer.URL+"/authorize?"+query.Encode() {
		t.Fatalf("pass-through authorize: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {"kc-code-direct"}, "client_id": {"desktop"}, "code_verifier": {strings.Repeat("b", 43)}, "redirect_uri": {"http://127.0.0.1:7000/cb"}}
	rec = proxyRequest(handler, "POST", "https://postra.test/oauth/token", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"refresh_token":"kc-refresh"`) || len(f.upstream) != 1 || f.upstream[0].Get("code_verifier") != strings.Repeat("b", 43) || f.upstream[0].Get("client_id") != "desktop" {
		t.Fatalf("pass-through token: %d %s upstream=%v", rec.Code, rec.Body.String(), f.upstream)
	}
	rec = proxyRequest(handler, "POST", "https://postra.test/oauth/token", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "upstream says no") {
		t.Fatalf("upstream error not relayed: %d %s", rec.Code, rec.Body.String())
	}
	// The proxy's own upstream client and the web SSO client are never
	// pass-through identities.
	for _, id := range []string{"postra-mcp-proxy", "postra-web"} {
		query.Set("client_id", id)
		if rec := proxyRequest(handler, "GET", "https://postra.test/oauth/authorize?"+query.Encode(), "", nil); rec.Code != 400 {
			t.Fatalf("%s treated as a client: %d", id, rec.Code)
		}
	}
}

func TestMCPOAuthProxyRateLimitsRegistration(t *testing.T) {
	f := newProxyFixture(t)
	handler := OAuthProxyHandler(f.app)
	last := 0
	for i := 0; i < 65; i++ {
		req := httptest.NewRequest("POST", "https://postra.test/oauth/register", strings.NewReader(`{"redirect_uris":["https://flood.example/cb"]}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.9:4000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != 429 {
		t.Fatalf("65th registration from one address answered %d", last)
	}
	req := httptest.NewRequest("POST", "https://postra.test/oauth/register", strings.NewReader(`{"redirect_uris":["https://other.example/cb"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:4000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("another address blocked: %d", rec.Code)
	}
}
