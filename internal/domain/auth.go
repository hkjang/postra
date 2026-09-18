package domain

import "time"

// UserRole controls Postra management permissions. Every authenticated user
// owns an isolated mail workspace; admins additionally manage identities.
type UserRole string

const (
	RoleAdmin UserRole = "admin"
	RoleUser  UserRole = "user"
)

type UserStatus string

const (
	UserActive   UserStatus = "active"
	UserDisabled UserStatus = "disabled"
	UserDeleted  UserStatus = "deleted"
)

type User struct {
	ID           string     `json:"id"`
	LoginID      string     `json:"login_id"`
	DisplayName  string     `json:"display_name"`
	Email        string     `json:"email,omitempty"`
	Role         UserRole   `json:"role"`
	Status       UserStatus `json:"status"`
	AuthProvider string     `json:"auth_provider"` // local | oidc
	OIDCIssuer   string     `json:"oidc_issuer,omitempty"`
	OIDCSubject  string     `json:"oidc_subject,omitempty"`
	CreatedAt    int64      `json:"created_at"`
	UpdatedAt    int64      `json:"updated_at"`
	LastLoginAt  int64      `json:"last_login_at,omitempty"`
}

type Session struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	TokenHash string `json:"-"`
	CSRFHash  string `json:"-"`
	ExpiresAt int64  `json:"expires_at"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	UserAgent string `json:"-"`
	IPAddress string `json:"-"`
}

type Principal struct {
	UserID         string    `json:"user_id"`
	LoginID        string    `json:"login_id"`
	DisplayName    string    `json:"display_name"`
	Role           UserRole  `json:"role"`
	AuthMethod     string    `json:"auth_method"` // local | oidc | api_token | mcp_key | mcp_oauth | cli
	MCPKeyID       string    `json:"mcp_key_id,omitempty"`
	MCPScopes      []string  `json:"mcp_scopes,omitempty"`
	OAuthClientID  string    `json:"-"`
	OAuthIssuer    string    `json:"-"`
	OAuthExpiresAt time.Time `json:"-"`
}

func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

// IsMCPScoped identifies delegated credentials, regardless of transport.
func (p Principal) IsMCPScoped() bool {
	return p.AuthMethod == "mcp_key" || p.AuthMethod == "mcp_oauth"
}

type MCPKey struct {
	ID           string   `json:"id"`
	UserID       string   `json:"user_id"`
	Name         string   `json:"name"`
	KeyHash      string   `json:"-"`
	KeyPrefix    string   `json:"key_prefix"`
	Status       string   `json:"status"` // active | revoked
	CreatedAt    int64    `json:"created_at"`
	LastUsedAt   int64    `json:"last_used_at,omitempty"`
	Scopes       []string `json:"scopes"`
	LegacyScopes bool     `json:"legacy_scopes,omitempty"`
}

// MCPOAuthClient is an OAuth client that registered itself (RFC 7591) with
// the Postra MCP authorization-server proxy. It carries no user identity or
// mail authority: end users still sign in at Keycloak and every token is
// checked against the same MCP resource policy as a pre-registered client.
type MCPOAuthClient struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name"`
	SecretHash   string   `json:"-"` // empty for public clients
	RedirectURIs []string `json:"redirect_uris"`
	AuthMethod   string   `json:"token_endpoint_auth_method"` // none | client_secret_basic | client_secret_post
	CreatedAt    int64    `json:"created_at"`
	LastUsedAt   int64    `json:"last_used_at,omitempty"`
}
