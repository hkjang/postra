package domain

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
	UserID      string   `json:"user_id"`
	LoginID     string   `json:"login_id"`
	DisplayName string   `json:"display_name"`
	Role        UserRole `json:"role"`
	AuthMethod  string   `json:"auth_method"` // local | oidc | api_token | mcp_key | cli
}

func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

type MCPKey struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	Name       string `json:"name"`
	KeyHash    string `json:"-"`
	KeyPrefix  string `json:"key_prefix"`
	Status     string `json:"status"` // active | revoked
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
}
