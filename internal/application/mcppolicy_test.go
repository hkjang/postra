package application

import "testing"

func TestMCPPolicyEvaluate(t *testing.T) {
	p := &MCPPolicy{
		Default:   "allow",
		DenyTools: []string{"mail_server_delete"},
		RoleDeny:  map[string][]string{"user": {"mail_send"}},
		RoleAllow: map[string][]string{"readonly": {"mail_search", "mail_message_get"}},
	}
	cases := []struct {
		role, tool string
		want       bool
	}{
		{"admin", "mail_search", true},
		{"admin", "mail_server_delete", false}, // hard deny
		{"user", "mail_send", false},           // role deny
		{"user", "mail_search", true},
		{"readonly", "mail_search", true}, // role allowlist
		{"readonly", "mail_send", false},  // not in allowlist
	}
	for _, c := range cases {
		got, _ := p.evaluate(c.role, c.tool)
		if got != c.want {
			t.Errorf("evaluate(%s,%s)=%v want %v", c.role, c.tool, got, c.want)
		}
	}
}

func TestMCPPolicyDefaultDeny(t *testing.T) {
	p := &MCPPolicy{Default: "deny", AllowTools: []string{"mail_search"}}
	if ok, _ := p.evaluate("user", "mail_search"); !ok {
		t.Error("allowlisted tool should pass under default deny")
	}
	if ok, _ := p.evaluate("user", "mail_send"); ok {
		t.Error("non-allowlisted tool should be denied under default deny")
	}
}

func TestMCPPolicyNil(t *testing.T) {
	var p *MCPPolicy
	if ok, _ := p.evaluate("user", "anything"); !ok {
		t.Error("nil policy should allow all")
	}
}
