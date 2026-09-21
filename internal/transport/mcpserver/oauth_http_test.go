package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"postra/internal/application"
	"postra/internal/domain"
)

type mcpOAuthFixture struct {
	app    *application.App
	admin  context.Context
	issuer *httptest.Server
	key    *rsa.PrivateKey
	// tokenEndpoint, when set, answers POST /token for proxy tests.
	tokenEndpoint http.HandlerFunc
}

func newMCPOAuthFixture(t *testing.T, endpoints ...string) *mcpOAuthFixture {
	t.Helper()
	endpoint := "/mcp"
	if len(endpoints) > 0 {
		endpoint = endpoints[0]
	}
	app, admin, _ := convergenceApp(t, map[string]string{"mcp.endpoint": endpoint})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &mcpOAuthFixture{app: app, admin: admin, key: key}
	f.issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.issuer.URL, "jwks_uri": f.issuer.URL + "/keys", "authorization_endpoint": f.issuer.URL + "/authorize", "token_endpoint": f.issuer.URL + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			if f.tokenEndpoint == nil {
				http.NotFound(w, r)
				return
			}
			f.tokenEndpoint(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.issuer.Close)
	f.patch(t, map[string]string{application.SettingOIDCIssuer: f.issuer.URL, application.SettingOIDCClientID: "postra-web", application.SettingOIDCRedirectURL: "https://postra.test/auth/oidc/callback", "mcp.oauth.enabled": "true", "mcp.oauth.resource_url": "https://postra.test" + endpoint, "mcp.oauth.allowed_client_ids": "desktop,another-desktop", "mcp.oauth.allowed_scopes": "mail.read,mail.search,mail.draft,mail.send"})
	user, err := app.Store.GetUser(admin, application.DefaultUserID)
	if err != nil {
		t.Fatal(err)
	}
	user.AuthProvider, user.OIDCIssuer, user.OIDCSubject = "oidc", f.issuer.URL, "owner-subject"
	if err := app.Store.UpdateUser(admin, user); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *mcpOAuthFixture) patch(t *testing.T, values map[string]string) {
	t.Helper()
	if _, err := f.app.AdminPatchSettings(f.admin, application.SettingsPatch{Values: values}); err != nil {
		t.Fatal(err)
	}
}

func (f *mcpOAuthFixture) token(t *testing.T, overrides map[string]any) string {
	t.Helper()
	claims := map[string]any{"iss": f.issuer.URL, "sub": "owner-subject", "aud": "https://postra.test/mcp", "azp": "desktop", "typ": "Bearer", "scope": "openid mail.read mail.search", "exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix()}
	for key, value := range overrides {
		claims[key] = value
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture").WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestMCPOAuthMetadataAndChallengesUseConfiguredOriginAndLivePolicy(t *testing.T) {
	f := newMCPOAuthFixture(t)
	for _, handler := range []http.Handler{OAuthMetadataHandler(f.app), HTTPHandler(f.app, "")} {
		for _, path := range []string{OAuthMetadataPath, OAuthMetadataPath + "/mcp"} {
			req := httptest.NewRequest("GET", "https://attacker.test"+path, nil)
			req.Header.Set("Forwarded", "host=attacker.test;proto=http")
			req.Header.Set("Origin", "https://browser-client.test")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			var metadata struct {
				Resource             string   `json:"resource"`
				AuthorizationServers []string `json:"authorization_servers"`
				Scopes               []string `json:"scopes_supported"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &metadata); err != nil || rec.Code != 200 || metadata.Resource != "https://postra.test/mcp" || len(metadata.AuthorizationServers) != 1 || metadata.AuthorizationServers[0] != f.issuer.URL {
				t.Fatalf("invalid public metadata: status=%d, err=%v", rec.Code, err)
			}
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" || strings.Contains(rec.Body.String(), "attacker.test") || strings.Contains(rec.Body.String(), "allowed_client_ids") {
				t.Fatal("metadata origin or disclosure policy failed")
			}
		}
	}
	handler := HTTPHandler(f.app, "")
	f.patch(t, map[string]string{"mcp.permissions.search": "false"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "https://attacker.test/mcp", nil))
	challenge := rec.Header().Get("WWW-Authenticate")
	if rec.Code != 401 || !strings.Contains(challenge, `resource_metadata="https://postra.test/.well-known/oauth-protected-resource/mcp"`) || !strings.Contains(challenge, `scope="mail.read"`) || strings.Contains(challenge, "mail.search") || strings.Contains(challenge, "mail.send") {
		t.Fatalf("wrong OAuth bootstrap challenge: status=%d %q", rec.Code, challenge)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "https://postra.test"+OAuthMetadataPath+"/mcp", nil))
	if strings.Contains(rec.Body.String(), "mail.search") {
		t.Fatal("metadata retained revoked capability")
	}
	for _, path := range []string{OAuthMetadataPath + "/unknown", OAuthMetadataPath + "/mcp/extra"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "https://postra.test"+path, nil))
		if rec.Code != 404 {
			t.Fatal("unconfigured resource metadata exposed")
		}
	}
	f.patch(t, map[string]string{"mcp.oauth.enabled": "false"})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "https://postra.test"+OAuthMetadataPath, nil))
	if rec.Code != 404 {
		t.Fatal("disabled OAuth advertised")
	}
}

func TestMCPOAuthRejectsInvalidTokensOnEveryHTTPMethodWithoutFallback(t *testing.T) {
	f := newMCPOAuthFixture(t)
	// OAuth alone must enforce authentication even on trusted-local installs.
	f.app.Cfg.Auth.Enabled = false
	handler := HTTPHandler(f.app, "")
	for label, bearer := range map[string]string{
		"missing": "", "garbage": "Bearer private-invalid-token", "basic": "Basic private-invalid-token",
		"multiple": "Bearer first second", "id-token": "Bearer " + f.token(t, map[string]any{"typ": "ID"}),
		"web-audience":   "Bearer " + f.token(t, map[string]any{"aud": "postra-web", "azp": "postra-web"}),
		"unknown-client": "Bearer " + f.token(t, map[string]any{"azp": "unknown"}),
		"expired":        "Bearer " + f.token(t, map[string]any{"exp": time.Now().Add(-time.Second).Unix()}),
	} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			t.Run(label+"/"+method, func(t *testing.T) {
				req := httptest.NewRequest(method, "https://postra.test/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
				if bearer != "" {
					req.Header.Set("Authorization", bearer)
				}
				req.AddCookie(&http.Cookie{Name: "postra_session", Value: "not-an-MCP-credential"})
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata=") || strings.Contains(rec.Body.String(), "private-invalid-token") || strings.Contains(rec.Body.String(), "eyJ") {
					t.Fatalf("unsafe authentication response: %s %s status=%d", label, method, rec.Code)
				}
			})
		}
	}
	f.patch(t, map[string]string{"mcp.oauth.enabled": "false"})
	req := httptest.NewRequest("POST", "https://postra.test/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+f.token(t, nil))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 || strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatal("explicit bearer downgraded to trusted-local authorization")
	}
}

func TestMCPOAuthMetadataSupportsCustomEndpointOnPrimaryAndDedicatedListeners(t *testing.T) {
	f := newMCPOAuthFixture(t, "/agent/mail")
	main := http.NewServeMux()
	main.Handle("/agent/mail", HTTPHandler(f.app, ""))
	metadata := OAuthMetadataHandler(f.app)
	main.Handle(OAuthMetadataPath, metadata)
	main.Handle(OAuthMetadataPath+"/", metadata)
	for _, handler := range []http.Handler{main, HTTPHandler(f.app, "")} {
		for _, path := range []string{OAuthMetadataPath, OAuthMetadataPath + "/agent/mail"} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest("GET", "https://proxy-untrusted.test"+path, nil))
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"resource":"https://postra.test/agent/mail"`) {
				t.Fatalf("custom-endpoint metadata missing: %d", rec.Code)
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "https://postra.test/agent/mail", nil))
		if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "https://postra.test/.well-known/oauth-protected-resource/agent/mail") {
			t.Fatal("custom endpoint advertised the wrong metadata path")
		}
	}
}

func TestMCPOAuthSessionsBindClientAndEnforceLiveScopeAndOwnership(t *testing.T) {
	f := newMCPOAuthFixture(t)
	server := httptest.NewServer(HTTPHandler(f.app, ""))
	t.Cleanup(server.Close)
	client, transport := convergenceClient(t, server.URL, f.token(t, nil))
	p := mcpDecode[domain.Principal](t, mcpCall(t, client, "mail_identity", map[string]any{}))
	if p.UserID != application.DefaultUserID || p.AuthMethod != "mcp_oauth" || p.OAuthClientID != "" || p.OAuthIssuer != "" || !p.OAuthExpiresAt.IsZero() {
		t.Fatal("incorrect OAuth identity or hidden credential fields exposed")
	}
	mcpDecode[map[string]any](t, mcpCall(t, client, "mail_capabilities", map[string]any{}))
	mcpDecode[domain.MailAccount](t, mcpCall(t, client, "mail_account_get", map[string]any{"account_id": "acc_mcp"}))
	if _, err := client.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "postra://mail/nonexistent"}); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("SDK typed-nil resource error did not become a safe protocol error: %v", err)
	}
	requireToolError(t, mcpCall(t, client, "mail_account_get", map[string]any{"account_id": "acc_private"}), "not_found")
	requireToolError(t, mcpCall(t, client, "mail_draft", map[string]any{"account_id": "acc_mcp", "kind": "new"}), "insufficient_scope")
	transport.mu.Lock()
	sessionID := transport.sessionID
	transport.mu.Unlock()
	if sessionID == "" {
		t.Fatal("missing SDK session")
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		req, _ := http.NewRequest(method, server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":101,"method":"ping"}`))
		req.Header.Set("Authorization", "Bearer "+f.token(t, map[string]any{"azp": "another-desktop"}))
		req.Header.Set("Mcp-Session-Id", sessionID)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != 403 {
			t.Fatalf("different client resumed %s session: %d", method, response.StatusCode)
		}
	}
	// A same-client refreshed token may resume its session, but its new scope
	// set must replace (not union with) the initialization-time permissions.
	req, _ := http.NewRequest("POST", server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":102,"method":"tools/call","params":{"name":"mail_account_get","arguments":{"account_id":"acc_mcp"}}}`))
	req.Header.Set("Authorization", "Bearer "+f.token(t, map[string]any{"scope": "openid"}))
	req.Header.Set("Mcp-Session-Id", sessionID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), "insufficient_scope") {
		t.Fatalf("same-client scope downgrade ignored: status=%d err=%v", response.StatusCode, err)
	}
	f.patch(t, map[string]string{"mcp.oauth.allowed_scopes": "mail.search"})
	requireToolError(t, mcpCall(t, client, "mail_account_get", map[string]any{"account_id": "acc_mcp"}), "insufficient_scope")
	f.patch(t, map[string]string{"mcp.oauth.allowed_client_ids": "another-desktop"})
	if _, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "mail_identity", Arguments: map[string]any{}}); err == nil {
		t.Fatal("removed OAuth client retained session access")
	}
	_, key, err := f.app.CreateMCPKey(f.admin, "compatibility")
	if err != nil {
		t.Fatal(err)
	}
	keyClient, _ := convergenceClient(t, server.URL, key)
	if p := mcpDecode[domain.Principal](t, mcpCall(t, keyClient, "mail_identity", map[string]any{})); p.AuthMethod != "mcp_key" {
		t.Fatal("existing API key path regressed")
	}
}

func TestMCPOAuthTokenInfoExpiresAndSeparatesIssuerClientAndKey(t *testing.T) {
	expires := time.Now().Add(20 * time.Second)
	p := domain.Principal{UserID: "user", AuthMethod: "mcp_oauth", OAuthClientID: "desktop", OAuthIssuer: "https://issuer.test", OAuthExpiresAt: expires}
	first, err := mcpTokenInfo(application.WithPrincipal(context.Background(), p), "unused-private-bearer", nil)
	if err != nil || !first.Expiration.Equal(expires) || strings.Contains(first.UserID, "issuer.test") || strings.Contains(first.UserID, "unused-private-bearer") {
		t.Fatal("OAuth expiry/opaque binding failed")
	}
	for _, changed := range []domain.Principal{
		{UserID: "user", AuthMethod: "mcp_oauth", OAuthClientID: "other", OAuthIssuer: p.OAuthIssuer, OAuthExpiresAt: expires},
		{UserID: "user", AuthMethod: "mcp_oauth", OAuthClientID: p.OAuthClientID, OAuthIssuer: "https://other-issuer.test", OAuthExpiresAt: expires},
		{UserID: "user", AuthMethod: "mcp_key", MCPKeyID: "key"},
	} {
		next, err := mcpTokenInfo(application.WithPrincipal(context.Background(), changed), "", nil)
		if err != nil || next.UserID == first.UserID {
			t.Fatal("delegated identity collision")
		}
	}
}

func TestMCPOAuthScopeStepUpPreservesRequestAndAPIKeyErrorContract(t *testing.T) {
	f := newMCPOAuthFixture(t)
	f.patch(t, map[string]string{"mcp.oauth.allowed_scopes": "mail.read,mail.search,mail.draft,mail.ai,mail.work,mail.delete", "mcp.permissions.delete": "true"})
	server := httptest.NewServer(HTTPHandler(f.app, ""))
	t.Cleanup(server.Close)
	client, transport := convergenceClient(t, server.URL+"/mcp", f.token(t, nil))
	transport.mu.Lock()
	sessionID := transport.sessionID
	transport.mu.Unlock()
	call := func(token, body string) (int, string, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", server.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Mcp-Session-Id", sessionID)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, res.Header.Get("WWW-Authenticate"), string(raw)
	}
	const accountCall = `{"jsonrpc":"2.0","id":201,"method":"tools/call","params":{"name":"mail_account_get","arguments":{"account_id":"acc_mcp"}}}`
	for _, test := range []struct{ body, scopes string }{
		{accountCall, "mail.read"},
		{`{"jsonrpc":"2.0","id":202,"method":"tools/call","params":{"name":"mail_draft","arguments":{"account_id":"acc_mcp","kind":"new","instructions":"Write a reply"}}}`, "mail.draft mail.ai"},
		{`{"jsonrpc":"2.0","id":203,"method":"tools/call","params":{"name":"mail_batch_update","arguments":{"action":"delete","message_ids":[]}}}`, "mail.work mail.delete"},
		{`{"jsonrpc":"2.0","id":204,"method":"resources/read","params":{"uri":"postra://draft/nonexistent"}}`, "mail.draft"},
	} {
		code, challenge, body := call(f.token(t, map[string]any{"scope": "openid"}), test.body)
		if code != 200 || !strings.Contains(challenge, `error="insufficient_scope"`) || !strings.Contains(challenge, `scope="`+test.scopes+`"`) || !strings.Contains(challenge, `resource_metadata="https://postra.test/.well-known/oauth-protected-resource/mcp"`) || !strings.Contains(body, "insufficient_scope") {
			t.Fatalf("missing bounded OAuth step-up challenge: status=%d challenge=%q body=%s", code, challenge, body)
		}
	}
	audits, err := f.app.Store.SearchAudit(f.admin, application.DefaultUserID, 100)
	if err != nil {
		t.Fatal(err)
	}
	denials := 0
	for _, event := range audits {
		if event.Action == "mcp_call" && event.Result == "error" && strings.Contains(event.Detail, `"error_code":"insufficient_scope"`) {
			denials++
			if !strings.Contains(event.Detail, `"oauth_client_id":"desktop"`) || !strings.Contains(event.Detail, `"auth_method":"mcp_oauth"`) || strings.Contains(event.Detail, "Write a reply") || strings.Contains(event.Detail, "eyJ") {
				t.Fatal("scope challenge audit lost client identity or disclosed content")
			}
		}
	}
	if denials != 4 {
		t.Fatalf("HTTP step-up attempts missing from MCP call audit: %d", denials)
	}
	code, challenge, body := call(f.token(t, map[string]any{"scope": "mail.read"}), accountCall)
	if code != 200 || challenge != "" || !strings.Contains(body, "acc_mcp") || strings.Contains(body, "insufficient_scope") {
		t.Fatal("same-client scope upgrade did not resume its session")
	}
	// Read-ahead must not lose the body when the request exceeds its bound;
	// the SDK's shared policy still prevents protected operations.
	oversized := `{"jsonrpc":"2.0","id":205,"method":"tools/call","params":{"name":"mail_account_get","arguments":{"account_id":"acc_mcp","padding":"` + strings.Repeat("x", oauthPreflightLimit) + `"}}}`
	code, challenge, body = call(f.token(t, map[string]any{"scope": "openid"}), oversized)
	if code != 200 || challenge != "" || !strings.Contains(body, "insufficient_scope") {
		t.Fatal("oversized OAuth request skipped the SDK policy or changed its body")
	}
	f.patch(t, map[string]string{"mcp.permissions.read": "false"})
	code, challenge, body = call(f.token(t, nil), accountCall)
	if code != 200 || challenge != "" || !strings.Contains(body, "insufficient_scope") {
		t.Fatal("administrator-disabled scope incorrectly triggered a consent loop")
	}
	f.patch(t, map[string]string{"mcp.permissions.read": "true"})
	_, key, err := f.app.CreateMCPKey(f.admin, "key-contract")
	if err != nil {
		t.Fatal(err)
	}
	keyClient, _ := convergenceClient(t, server.URL+"/mcp", key)
	requireToolError(t, mcpCall(t, keyClient, "mail_draft", map[string]any{"account_id": "acc_mcp", "kind": "new"}), "insufficient_scope")
	// The original token's read permission was never upgraded by another
	// token used with this session: every request carries its own authority.
	requireToolError(t, mcpCall(t, client, "mail_account_get", map[string]any{"account_id": "acc_private"}), "not_found")
}

type failedOAuthBody struct{}

func (failedOAuthBody) Read(p []byte) (int, error) {
	return copy(p, `{"jsonrpc":"2.0","id":1,"method":"ping"}`), errors.New("private transport error")
}
func (failedOAuthBody) Close() error { return nil }

func TestMCPOAuthScopeHintMalformedAndLargeBodiesArePreserved(t *testing.T) {
	f := newMCPOAuthFixture(t)
	info := f.app.MCPOAuthConnection().OAuth
	p := domain.Principal{UserID: application.DefaultUserID, AuthMethod: "mcp_oauth", MCPScopes: []string{}}
	for _, body := range []string{`invalid-json`, `[]`, `{"jsonrpc":"2.0","id":1,"method":"unknown"}`, strings.Repeat("x", oauthPreflightLimit+10)} {
		req := httptest.NewRequest("POST", "https://postra.test/mcp", strings.NewReader(body))
		req = req.WithContext(application.WithPrincipal(req.Context(), p))
		if oauthScopeHint(httptest.NewRecorder(), req, f.app, info) {
			t.Fatal("preflight replaced SDK protocol validation")
		}
		got, err := io.ReadAll(req.Body)
		if err != nil || string(got) != body {
			t.Fatal("preflight changed request bytes")
		}
	}
	req := httptest.NewRequest("POST", "https://postra.test/mcp", nil)
	req.Body = failedOAuthBody{}
	rec := httptest.NewRecorder()
	if !oauthScopeHint(rec, req, f.app, info) || rec.Code != 400 || strings.Contains(rec.Body.String(), "private transport error") {
		t.Fatal("body read failure was swallowed or disclosed")
	}
}
