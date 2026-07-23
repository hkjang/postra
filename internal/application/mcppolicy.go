package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"postra/internal/domain"
)

// accessLevel classifies an MCP tool by privilege/risk. Levels are ordered:
// a role authorized up to level N may call every tool at levels ≤ N.
type accessLevel int

const (
	AccessRead   accessLevel = iota // safe reads + AI analysis (no mutation)
	AccessWrite                     // reversible local mutation (drafts, labels, rules, cards, collab)
	AccessSend                      // outbound send + ingest sync
	AccessDelete                    // irreversible local deletion
	AccessAdmin                     // credentials, account config, server-side deletion
)

func (l accessLevel) String() string {
	switch l {
	case AccessRead:
		return "read"
	case AccessWrite:
		return "write"
	case AccessSend:
		return "send"
	case AccessDelete:
		return "delete"
	case AccessAdmin:
		return "admin"
	default:
		return "unknown"
	}
}

func parseLevel(s string) (accessLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read":
		return AccessRead, true
	case "write":
		return AccessWrite, true
	case "send":
		return AccessSend, true
	case "delete":
		return AccessDelete, true
	case "admin":
		return AccessAdmin, true
	default:
		return AccessRead, false
	}
}

// toolAccess is the authoritative risk classification of every MCP tool. Any
// tool NOT listed here is treated as AccessAdmin (admin-only) — a fail-safe so
// a newly-added, unclassified tool is never silently exposed to regular users.
var toolAccess = map[string]accessLevel{
	// ---- read (safe) ----
	"mail_account_list":            AccessRead,
	"mail_account_get":             AccessRead,
	"mail_account_test":            AccessRead,
	"job_status":                   AccessRead,
	"mail_search":                  AccessRead,
	"mail_hybrid_search":           AccessRead,
	"mail_work_inbox":              AccessRead,
	"mail_message_get":             AccessRead,
	"mail_thread_get":              AccessRead,
	"mail_thread_timeline":         AccessRead,
	"mail_attachment_list":         AccessRead,
	"mail_attachment_extract_text": AccessRead,
	"mail_summarize":               AccessRead,
	"mail_classify":                AccessRead,
	"mail_action_items_extract":    AccessRead,
	"mail_entities_extract":        AccessRead,
	"mail_phishing_inspect":        AccessRead,
	"mail_auth_inspect":            AccessRead,
	"mail_thread_summarize":        AccessRead,
	"mail_question_answer":         AccessRead,
	"mail_semantic_search":         AccessRead,
	"mail_attachment_summarize":    AccessRead,
	"mail_eval_prompt":             AccessRead,
	"mail_send_preview":            AccessRead,
	"mail_outbound_status":         AccessRead,
	"mail_audit_search":            AccessRead,
	"mail_rules_list":              AccessRead,
	"mail_action_cards_list":       AccessRead,
	"mail_team_inbox":              AccessRead,
	"mail_collab_get":              AccessRead,
	"secret_registration_begin":    AccessRead, // returns instructions only
	"secret_rotation_begin":        AccessRead, // returns instructions only

	// ---- write (reversible local mutation) ----
	"mail_sync_start":             AccessWrite,
	"job_cancel":                  AccessWrite,
	"mail_embeddings_build":       AccessWrite,
	"mail_draft_create":           AccessWrite,
	"mail_draft_update":           AccessWrite,
	"mail_draft_rewrite":          AccessWrite,
	"mail_batch_update":           AccessWrite,
	"mail_rule_create":            AccessWrite,
	"mail_rule_update":            AccessWrite,
	"mail_rule_delete":            AccessWrite,
	"mail_apply_rules":            AccessWrite,
	"mail_action_cards_extract":   AccessWrite,
	"mail_action_card_set_status": AccessWrite,
	"mail_action_card_export":     AccessWrite,
	"mail_assign":                 AccessWrite,
	"mail_set_work_status":        AccessWrite,
	"mail_add_note":               AccessWrite,

	// ---- send (outbound) ----
	"mail_send_request_approval": AccessSend,
	"mail_send":                  AccessSend,

	// ---- delete (irreversible local) ----
	"mail_local_delete": AccessDelete,

	// ---- admin (credentials / account config / server-side destruction) ----
	"mail_account_create":                 AccessAdmin,
	"mail_account_update":                 AccessAdmin,
	"mail_account_disable":                AccessAdmin,
	"secret_revoke":                       AccessAdmin,
	"mail_server_delete_preview":          AccessAdmin,
	"mail_server_delete_request_approval": AccessAdmin,
	"mail_server_delete":                  AccessAdmin,
}

// classifyTool returns a tool's risk level, defaulting unclassified tools to
// AccessAdmin (fail-safe).
func classifyTool(tool string) accessLevel {
	if l, ok := toolAccess[tool]; ok {
		return l
	}
	return AccessAdmin
}

// ClassifyMCPTool reports a tool's built-in access level and whether it is
// explicitly classified (used by drift tests).
func ClassifyMCPTool(tool string) (string, bool) {
	l, ok := toolAccess[tool]
	return l.String(), ok
}

// builtinRoleMax is the default maximum access level per role. Admins get
// everything; regular users (personal MCP key) are capped at outbound send —
// they can search, read, analyze, draft, organize, and send with approval, but
// cannot delete mail, manage credentials/accounts, or run server-side
// deletion. Unknown roles are read-only.
func builtinRoleMax(role string) accessLevel {
	switch role {
	case string(domain.RoleAdmin):
		return AccessAdmin
	case string(domain.RoleUser):
		return AccessSend
	default:
		return AccessRead
	}
}

// MCPPolicy is the optional administrator override layered on top of the
// built-in RBAC defaults (§MCP 정책 게이트웨이). Every field is optional; an empty
// or absent policy means "use the built-in role model".
type MCPPolicy struct {
	// RoleMaxLevel overrides the default maximum access level for a role
	// (read|write|send|delete|admin).
	RoleMaxLevel map[string]string `json:"role_max_level,omitempty"`
	// RoleAllow, when set for a role, is an exact allowlist that fully governs
	// that role (ignores level defaults for it).
	RoleAllow map[string][]string `json:"role_allow,omitempty"`
	// RoleDeny denies specific tools for a role, above any allowance.
	RoleDeny map[string][]string `json:"role_deny,omitempty"`
	// DenyTools are denied for everyone (hard deny, checked first).
	DenyTools []string `json:"deny_tools,omitempty"`
	// AllowTools escalates specific tools to any role regardless of level.
	AllowTools []string `json:"allow_tools,omitempty"`
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// evaluateAccess is the full RBAC decision for (role, tool), layering the
// optional policy over the built-in level model. Order: hard deny → role deny
// → role allowlist → (built-in or overridden) level → escalation allowlist.
func evaluateAccess(pol *MCPPolicy, role, tool string) (bool, string) {
	lvl := classifyTool(tool)
	if pol != nil {
		if contains(pol.DenyTools, tool) {
			return false, "tool is on the deny list"
		}
		if contains(pol.RoleDeny[role], tool) {
			return false, "tool is denied for role " + role
		}
		if allow, ok := pol.RoleAllow[role]; ok && len(allow) > 0 {
			if contains(allow, tool) {
				return true, ""
			}
			return false, "tool is not in the allowlist for role " + role
		}
	}
	max := builtinRoleMax(role)
	if pol != nil {
		if s, ok := pol.RoleMaxLevel[role]; ok {
			if parsed, ok2 := parseLevel(s); ok2 {
				max = parsed
			}
		}
	}
	if lvl <= max {
		return true, ""
	}
	if pol != nil && contains(pol.AllowTools, tool) {
		return true, ""
	}
	return false, fmt.Sprintf("role %q is limited to %q access but tool requires %q", role, max, lvl)
}

// mcpPolicyState caches the parsed override policy, refreshed on settings changes.
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
// is always allowed. Remote callers (personal MCP key, OIDC, or API token) are
// checked against the role model. Denials are audited.
func (a *App) CheckMCPToolPolicy(ctx context.Context, tool string) error {
	p, ok := PrincipalFrom(ctx)
	if !ok || p.UserID == "" {
		return nil // local/trusted stdio
	}
	role := string(p.Role)
	if allowed, reason := evaluateAccess(a.currentMCPPolicy(), role, tool); !allowed {
		a.audit(ctx, "mcp_policy_denied", "tool:"+tool, "denied", reason)
		return userErrf("MCP access denied for tool %q: %s", tool, reason)
	}
	return nil
}
