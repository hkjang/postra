package config

import "testing"

func TestDefaultUsesSingleHTTPPort(t *testing.T) {
	cfg := Default()
	if cfg.HTTPAddr != "127.0.0.1:8480" {
		t.Fatalf("HTTPAddr = %q, want 127.0.0.1:8480", cfg.HTTPAddr)
	}
	if cfg.MCPHTTPAddr != "" {
		t.Fatalf("MCPHTTPAddr = %q, want empty dedicated listener by default", cfg.MCPHTTPAddr)
	}
}
