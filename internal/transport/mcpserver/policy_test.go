package mcpserver

import (
	"testing"

	"postra/internal/application"
)

// TestEveryCatalogToolClassified guards against drift: every tool advertised in
// the catalog must have an explicit RBAC access level, so no tool is left as
// the admin-only fail-safe default by accident.
func TestEveryCatalogToolClassified(t *testing.T) {
	summary := toolCatalogSummary()
	groups, ok := summary["groups"].(map[string][]string)
	if !ok {
		t.Fatalf("catalog groups have unexpected type %T", summary["groups"])
	}
	seen := 0
	for group, tools := range groups {
		for _, tool := range tools {
			seen++
			if _, known := application.ClassifyMCPTool(tool); !known {
				t.Errorf("tool %q (group %q) has no RBAC access-level classification", tool, group)
			}
		}
	}
	if seen == 0 {
		t.Fatal("catalog is empty")
	}
}
