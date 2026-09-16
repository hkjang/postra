package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"postra/internal/application"
	"postra/internal/domain"
)

const OAuthMetadataPath = "/.well-known/oauth-protected-resource"

// OAuthMetadataHandler serves only public, operator-configured resource
// metadata. Never construct authorization URLs from untrusted proxy headers.
// The same handler is mounted on the primary and optional MCP listeners.
func OAuthMetadataHandler(app *application.App) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		info := app.MCPOAuthConnection().OAuth
		metadataURL, err := url.Parse(info.MetadataURL)
		if !app.SettingBool("mcp.enabled") || !app.SettingBool("mcp.http_enabled") || !info.Enabled || !info.Configured || err != nil || metadataURL.Host == "" || (r.URL.Path != OAuthMetadataPath && r.URL.Path != metadataURL.Path) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Allow", "GET, OPTIONS")
		auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               info.ResourceURL,
			AuthorizationServers:   []string{info.Issuer},
			ScopesSupported:        append([]string{}, info.ScopesSupported...),
			BearerMethodsSupported: []string{"header"},
			ResourceName:           "Postra MCP",
		}).ServeHTTP(w, r)
	})
}

func oauthBearerOptions(info application.MCPOAuthInfo) *auth.RequireBearerTokenOptions {
	if !info.Enabled || !info.Configured {
		return nil
	}
	// Scopes are checked per tool, not at connection/identity discovery time.
	return &auth.RequireBearerTokenOptions{ResourceMetadataURL: info.MetadataURL}
}

func oauthChallenge(info application.MCPOAuthInfo, invalid bool) string {
	parts := []string{}
	if info.Enabled && info.Configured {
		parts = append(parts, fmt.Sprintf("resource_metadata=%q", info.MetadataURL))
		defaults := []string{}
		for _, scope := range application.DefaultMCPScopes() {
			if slices.Contains(info.ScopesSupported, scope) {
				defaults = append(defaults, scope)
			}
		}
		if len(defaults) > 0 {
			parts = append(parts, fmt.Sprintf("scope=%q", strings.Join(defaults, " ")))
		}
	}
	if invalid {
		parts = append(parts, `error="invalid_token"`)
	}
	if len(parts) == 0 {
		return "Bearer"
	}
	return "Bearer " + strings.Join(parts, ", ")
}

func writeMCPAuthenticationError(w http.ResponseWriter, ctx context.Context, info application.MCPOAuthInfo, invalid bool) {
	w.Header().Set("WWW-Authenticate", oauthChallenge(info, invalid))
	writeMCPHTTPError(w, ctx, &domain.PublicError{Code: "unauthorized", Message: "MCP 인증이 필요하거나 인증 정보가 유효하지 않습니다.", Status: 401})
}

const oauthPreflightLimit = 1 << 20

type replayRequestBody struct {
	io.Reader
	io.Closer
}

// Standard OAuth clients discover additional scopes via an HTTP 403
// challenge, before the SDK commits a JSON/SSE response. Read-ahead is bounded
// and replays the exact bytes; oversized or malformed requests are left to
// the SDK, whose shared gateway always remains authoritative.
func oauthScopePreflight(w http.ResponseWriter, r *http.Request, app *application.App, info application.MCPOAuthInfo) bool {
	if r.Method != http.MethodPost || r.URL.Path != app.MCPOAuthConnection().ActiveEndpoint || r.Body == nil {
		return false
	}
	started := time.Now()
	original := r.Body
	body, err := io.ReadAll(io.LimitReader(original, oauthPreflightLimit+1))
	r.Body = replayRequestBody{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}
	if err != nil {
		writeMCPHTTPError(w, r.Context(), &domain.PublicError{Code: "invalid_request", Message: "MCP 요청 본문을 읽을 수 없습니다.", Status: 400})
		return true
	}
	if len(body) > oauthPreflightLimit {
		return false
	}
	var request struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name      string          `json:"name"`
			URI       string          `json:"uri"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &request) != nil || request.Version != "2.0" || len(request.ID) == 0 {
		return false
	}
	tool := ""
	switch request.Method {
	case "tools/call":
		tool = application.CanonicalMCPTool(request.Params.Name)
		if _, known := application.ClassifyMCPTool(tool); !known {
			return false
		}
	case "resources/read":
		if request.Params.URI == "" {
			return false
		}
		tool = resourceTool(request.Params.URI)
	default:
		return false
	}
	var public *domain.PublicError
	err = checkMCPRequestPolicy(r.Context(), app, tool, request.Params.Arguments)
	if !errors.As(err, &public) || public.Code != "insufficient_scope" {
		return false
	}
	required, _ := public.Details["required_scopes"].([]string)
	for _, scope := range required {
		if !slices.Contains(info.ScopesSupported, scope) {
			// Consent cannot override an administrator's disabled capability.
			// Leave such denials to the normal SDK error contract, without
			// prompting an OAuth client to repeatedly request unavailable scopes.
			return false
		}
	}
	parts := []string{fmt.Sprintf("resource_metadata=%q", info.MetadataURL), `error="insufficient_scope"`}
	if len(required) > 0 {
		parts = append(parts, fmt.Sprintf("scope=%q", strings.Join(required, " ")))
	}
	w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(parts, ", "))
	writeMCPHTTPError(w, r.Context(), public)
	_, response := application.PublicError(r.Context(), public)
	encoded, _ := json.Marshal(response)
	inputBytes := len(request.Params.Arguments)
	if request.Method == "resources/read" {
		inputBytes = len(request.Params.URI)
	}
	recordMCPCall(r.Context(), app, tool, "error", public.Code, inputBytes, len(encoded)+1, time.Since(started))
	return true
}

// The SDK checks TokenInfo.UserID on every HTTP request that resumes a
// session. Bind OAuth to the issuer and authorized client, not just the user;
// token rotation for that same client still retains its own MCP session.
func mcpTokenInfo(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
	p, ok := application.PrincipalFrom(ctx)
	if !ok || p.UserID == "" {
		return nil, auth.ErrInvalidToken
	}
	identity := p.UserID + ":" + p.AuthMethod + ":" + p.MCPKeyID
	expires := time.Now().Add(time.Minute)
	if p.AuthMethod == "mcp_oauth" {
		if p.OAuthClientID == "" || p.OAuthIssuer == "" || p.OAuthExpiresAt.IsZero() {
			return nil, auth.ErrInvalidToken
		}
		binding := sha256.Sum256([]byte(p.OAuthIssuer + "\x00" + p.OAuthClientID))
		identity = fmt.Sprintf("%s:mcp_oauth:%x", p.UserID, binding)
		if p.OAuthExpiresAt.Before(expires) {
			expires = p.OAuthExpiresAt
		}
	}
	return &auth.TokenInfo{UserID: identity, Scopes: p.MCPScopes, Expiration: expires, Extra: map[string]any{"postra_principal": p, "postra_trace_id": application.RequestTrace(ctx)}}, nil
}
