package application

import (
	"context"
	"sort"
	"strings"

	"postra/internal/domain"
)

// Scope names are shared by per-client keys, transport authorization and tool
// metadata. Scopes are an additional restriction; they never grant a role.
var MCPScopes = []string{"mail.read", "mail.search", "mail.ai", "mail.draft", "mail.send", "mail.delete", "mail.work", "admin.read", "admin.write"}

var MCPToolAliases = map[string]string{
	"mail_accounts_list": "mail_account_list",
	"mail_get":           "mail_message_get",
	"mail_thread":        "mail_thread_get",
	"mail_draft":         "mail_draft_create",
	"mail_preview":       "mail_send_preview",
	"mail_approve":       "mail_send_request_approval",
	"mail_systeminfo":    "mail_system_info",
	"mail_ask":           "mail_question_answer",
}

func CanonicalMCPTool(tool string) string {
	if canonical, ok := MCPToolAliases[tool]; ok {
		return canonical
	}
	return tool
}

func DefaultMCPScopes() []string { return []string{"mail.read", "mail.search"} }

func NormalizeMCPScopes(scopes []string) ([]string, error) {
	if scopes == nil {
		return DefaultMCPScopes(), nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		scope := strings.TrimSpace(raw)
		if !contains(MCPScopes, scope) {
			return nil, &domain.PublicError{Code: "invalid_request", Message: "알 수 없는 MCP 권한 범위입니다.", Status: 400}
		}
		if !seen[scope] {
			out = append(out, scope)
			seen[scope] = true
		}
	}
	sort.Strings(out)
	return out, nil
}

func CheckMCPScopes(ctx context.Context, required ...string) error {
	p, ok := PrincipalFrom(ctx)
	if !ok || p.AuthMethod != "mcp_key" {
		return nil
	}
	for _, scope := range required {
		if !contains(p.MCPScopes, scope) {
			return &domain.PublicError{Code: "insufficient_scope", Message: "이 MCP 키에 필요한 권한이 없습니다.", Status: 403, Details: map[string]any{"required_scopes": required}}
		}
	}
	return nil
}

// CheckMCPPermissionScopes combines live administrator capability switches
// with per-key permissions. REST calls authenticated with MCP keys use this
// same guard so changing transport cannot bypass a disabled capability.
func (a *App) CheckMCPPermissionScopes(ctx context.Context, required ...string) error {
	if !a.SettingBool("mcp.enabled") {
		return &domain.PublicError{Code: "capability_disabled", Message: "MCP 기능이 비활성화되어 있습니다.", Status: 403}
	}
	if err := CheckMCPScopes(ctx, required...); err != nil {
		return err
	}
	for _, scope := range required {
		key := "mcp.permissions." + strings.ReplaceAll(strings.TrimPrefix(scope, "mail."), ".", "_")
		if !a.SettingBool(key) {
			return &domain.PublicError{Code: "capability_disabled", Message: "관리자 정책에서 해당 MCP 기능을 비활성화했습니다.", Status: 403, Details: map[string]any{"required_scopes": required}}
		}
	}
	return nil
}

func (a *App) checkMCPRuleActions(ctx context.Context, actions []domain.RuleAction) error {
	if p, ok := PrincipalFrom(ctx); !ok || p.AuthMethod != "mcp_key" {
		return nil
	}
	for _, action := range actions {
		if action.Type == domain.RuleActionDelete {
			return a.CheckMCPToolPolicy(ctx, "mail_local_delete")
		}
	}
	return nil
}

func (a *App) UpdateMCPKeyScopes(ctx context.Context, keyID string, scopes []string, admin bool) (*domain.MCPKey, error) {
	if p, ok := PrincipalFrom(ctx); ok && p.AuthMethod == "mcp_key" {
		return nil, &domain.PublicError{Code: "forbidden", Message: "MCP 키로 자격 증명 권한을 변경할 수 없습니다.", Status: 403}
	}
	if scopes == nil {
		return nil, &domain.PublicError{Code: "invalid_request", Message: "scopes 배열을 명시하세요. 빈 배열은 모든 도구 권한을 제거합니다.", Status: 400}
	}
	resolved, err := NormalizeMCPScopes(scopes)
	if err != nil {
		return nil, err
	}
	owner := userIDFrom(ctx)
	if admin {
		if _, err := requireAdmin(ctx); err != nil {
			return nil, err
		}
		owner = ""
	} else if p, ok := PrincipalFrom(ctx); ok && !p.IsAdmin() && (contains(resolved, "admin.read") || contains(resolved, "admin.write")) {
		return nil, &domain.PublicError{Code: "forbidden", Message: "관리자 권한 범위는 관리자만 부여할 수 있습니다.", Status: 403}
	}
	if err := a.Store.UpdateMCPKeyScopes(ctx, owner, keyID, resolved); err != nil {
		return nil, err
	}
	a.audit(ctx, "mcp_key_scopes_update", "mcp_key:"+keyID, "ok", strings.Join(resolved, ","))
	var keys []domain.MCPKey
	if admin {
		keys, err = a.Store.ListAllMCPKeys(ctx)
	} else {
		keys, err = a.Store.ListMCPKeys(ctx, owner)
	}
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		if key.ID == keyID {
			return &key, nil
		}
	}
	return nil, domain.ErrNotFound
}

// MCPToolScopes is fail-closed: an unknown tool requires explicit admin.write.
func MCPToolScopes(tool string) []string {
	tool = CanonicalMCPTool(tool)
	switch tool {
	case "mail_text_rewrite":
		return []string{"mail.draft", "mail.ai"}
	case "mail_drafts_list":
		return []string{"mail.draft"}
	case "mail_set_sla":
		return []string{"mail.work"}
	case "mail_action_card_create":
		return []string{"mail.work"}
	case "mail_capabilities", "mail_identity", "mail_system_info":
		return []string{}
	case "mail_search":
		return []string{"mail.search"}
	case "mail_hybrid_search", "mail_semantic_search":
		return []string{"mail.search", "mail.ai"}
	case "mail_summarize", "mail_classify", "mail_action_items_extract", "mail_entities_extract", "mail_phishing_inspect", "mail_thread_summarize", "mail_question_answer", "mail_suggest_replies", "mail_calendar_extract", "mail_daily_digest", "mail_rule_draft_from_text", "mail_attachment_summarize", "mail_eval_prompt", "mail_embeddings_build":
		return []string{"mail.ai"}
	case "mail_draft_create", "mail_draft_update", "mail_draft_get", "mail_draft_delete", "mail_draft_attachment_add", "mail_draft_attachment_get", "mail_draft_attachment_remove", "mail_render", "mail_signature_list", "mail_signature_get", "mail_signature_save", "mail_signature_delete", "mail_send_preview":
		return []string{"mail.draft"}
	case "mail_draft_rewrite":
		return []string{"mail.draft", "mail.ai"}
	case "mail_send", "mail_send_request_approval":
		return []string{"mail.send"}
	case "mail_local_delete", "mail_server_delete", "mail_server_delete_preview", "mail_server_delete_request_approval":
		return []string{"mail.delete"}
	case "mail_account_create", "mail_account_update", "mail_account_disable", "secret_revoke":
		return []string{"admin.write"}
	case "mail_audit_search":
		return []string{"admin.read"}
	case "mail_sync_start", "job_cancel", "mail_batch_update", "mail_rule_create", "mail_rule_update", "mail_rule_delete", "mail_apply_rules", "mail_action_card_set_status", "mail_action_card_export", "mail_assign", "mail_set_work_status", "mail_add_note":
		return []string{"mail.work"}
	case "mail_action_cards_extract":
		return []string{"mail.work", "mail.ai"}
	default:
		if level, known := ClassifyMCPTool(tool); known && level == "read" {
			return []string{"mail.read"}
		}
		return []string{"admin.write"}
	}
}
