package application

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// MCPPolicy is the central gateway policy for MCP tool access (§MCP 정책
// 게이트웨이). It is evaluated for every remote MCP tool call. Local stdio
// callers (no authenticated principal) are trusted and bypass the gateway.
type MCPPolicy struct {
	// Default action when no explicit rule matches: "allow" (default) | "deny".
	Default string `json:"default"`
	// AllowTools is the allowlist consulted when Default == "deny".
	AllowTools []string `json:"allow_tools,omitempty"`
	// DenyTools are always denied regardless of role.
	DenyTools []string `json:"deny_tools,omitempty"`
	// RoleAllow, when set for a role, restricts that role to exactly these tools.
	RoleAllow map[string][]string `json:"role_allow,omitempty"`
	// RoleDeny denies specific tools per role.
	RoleDeny map[string][]string `json:"role_deny,omitempty"`
}

// evaluate returns (allowed, reason). The order is: hard deny list → role deny
// → role allowlist → default policy.
func (p *MCPPolicy) evaluate(role, tool string) (bool, string) {
	if p == nil {
		return true, ""
	}
	if contains(p.DenyTools, tool) {
		return false, "tool is on the deny list"
	}
	if contains(p.RoleDeny[role], tool) {
		return false, "tool is denied for role " + role
	}
	if allow, ok := p.RoleAllow[role]; ok && len(allow) > 0 {
		if !contains(allow, tool) {
			return false, "tool is not in the allowlist for role " + role
		}
		return true, ""
	}
	if strings.EqualFold(p.Default, "deny") {
		if contains(p.AllowTools, tool) {
			return true, ""
		}
		return false, "default policy is deny and tool is not allowlisted"
	}
	return true, ""
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// mcpPolicyState caches the parsed policy, refreshed on settings changes.
type mcpPolicyState struct {
	mu     sync.RWMutex
	policy *MCPPolicy
}

func (a *App) loadMCPPolicy(ctx context.Context) {
	settings, err := a.Store.GetSettings(ctx)
	if err != nil {
		return
	}
	raw := strings.TrimSpace(settings[SettingMCPPolicy])
	var pol *MCPPolicy
	if raw != "" {
		var p MCPPolicy
		if json.Unmarshal([]byte(raw), &p) == nil {
			pol = &p
		}
	}
	a.mcpPolicy.mu.Lock()
	a.mcpPolicy.policy = pol
	a.mcpPolicy.mu.Unlock()
}

func (a *App) currentMCPPolicy() *MCPPolicy {
	a.mcpPolicy.mu.RLock()
	defer a.mcpPolicy.mu.RUnlock()
	return a.mcpPolicy.policy
}

// CheckMCPToolPolicy authorizes one MCP tool call for the calling principal.
// A request with no authenticated principal is a trusted local stdio call and
// is always allowed. Denials are audited.
func (a *App) CheckMCPToolPolicy(ctx context.Context, tool string) error {
	p, ok := PrincipalFrom(ctx)
	if !ok || p.UserID == "" {
		return nil // local/trusted
	}
	pol := a.currentMCPPolicy()
	if pol == nil {
		return nil
	}
	role := string(p.Role)
	if allowed, reason := pol.evaluate(role, tool); !allowed {
		a.audit(ctx, "mcp_policy_denied", "tool:"+tool, "denied", reason)
		return userErrf("MCP policy denied tool %q: %s", tool, reason)
	}
	return nil
}
