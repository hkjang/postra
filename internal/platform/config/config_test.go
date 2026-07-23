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

func TestPostgresDSNAutoDriver(t *testing.T) {
	t.Setenv("POSTRA_POSTGRES_DSN", "postgres://user:pass@localhost:5432/postra")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageDriver != "postgres" {
		t.Fatalf("StorageDriver = %q, want postgres when POSTRA_POSTGRES_DSN is set", cfg.StorageDriver)
	}
}

