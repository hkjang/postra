package application

import "testing"

// TestBuiltinRoleModel verifies the default (no config) RBAC: admins get every
// tier, users are capped at send, unknown roles are read-only.
func TestBuiltinRoleModel(t *testing.T) {
	cases := []struct {
		role, tool string
		want       bool
	}{
		// admin: everything
		{"admin", "mail_search", true},
		{"admin", "mail_send", true},
		{"admin", "mail_local_delete", true},
		{"admin", "mail_server_delete", true},
		{"admin", "secret_revoke", true},
		{"admin", "mail_account_create", true},

		// user: read/write/send yes; delete/admin no
		{"user", "mail_search", true},
		{"user", "mail_summarize", true},
		{"user", "mail_draft_create", true},
		{"user", "mail_batch_update", true},
		{"user", "mail_send", true},
		{"user", "mail_rule_create", true},
		{"user", "mail_assign", true},
		{"user", "mail_local_delete", false},
		{"user", "mail_server_delete", false},
		{"user", "secret_revoke", false},
		{"user", "mail_account_create", false},

		// unknown role: read-only
		{"guest", "mail_search", true},
		{"guest", "mail_draft_create", false},
		{"guest", "mail_send", false},

		// unclassified tool is admin-only (fail-safe)
		{"user", "mail_some_future_tool", false},
		{"admin", "mail_some_future_tool", true},
	}
	for _, c := range cases {
		got, reason := evaluateAccess(nil, c.role, c.tool)
		if got != c.want {
			t.Errorf("evaluateAccess(nil,%s,%s)=%v want %v (%s)", c.role, c.tool, got, c.want, reason)
		}
	}
}

func TestPolicyOverrides(t *testing.T) {
	// Grant a user local delete explicitly, deny them send, and raise a
	// read-only "guest" to write via RoleMaxLevel.
	pol := &MCPPolicy{
		RoleAllow:    map[string][]string{}, // none
		RoleDeny:     map[string][]string{"user": {"mail_send"}},
		AllowTools:   []string{"mail_local_delete"},
		RoleMaxLevel: map[string]string{"guest": "write"},
		DenyTools:    []string{"mail_eval_prompt"},
	}
	check := func(role, tool string, want bool) {
		t.Helper()
		if got, reason := evaluateAccess(pol, role, tool); got != want {
			t.Errorf("evaluateAccess(pol,%s,%s)=%v want %v (%s)", role, tool, got, want, reason)
		}
	}
	check("user", "mail_send", false)         // role deny wins over built-in send access
	check("user", "mail_local_delete", true)  // escalation allowlist
	check("guest", "mail_draft_create", true) // raised to write
	check("guest", "mail_send", false)        // still below send
	check("admin", "mail_eval_prompt", false) // hard deny applies to everyone
}

func TestRoleAllowlistGovernsRole(t *testing.T) {
	pol := &MCPPolicy{RoleAllow: map[string][]string{"user": {"mail_search", "mail_message_get"}}}
	if ok, _ := evaluateAccess(pol, "user", "mail_search"); !ok {
		t.Error("allowlisted tool should pass")
	}
	if ok, _ := evaluateAccess(pol, "user", "mail_draft_create"); ok {
		t.Error("non-allowlisted tool should be denied even though built-in would allow it")
	}
}
