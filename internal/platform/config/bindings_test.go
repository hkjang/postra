package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvironmentBindingsAndProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"ai":{"model":"file-model","timeout_sec":60},"sync":{"auto_sync_minutes":5}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSTRA_AI_MODEL", "env-model")
	t.Setenv("POSTRA_AI_TEMPERATURE", "0.45")
	t.Setenv("POSTRA_MAX_CONCURRENT_SYNCS", "4")
	t.Setenv("POSTRA_AI_STREAM", "false")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.Model != "env-model" || cfg.AI.Temperature != .45 || cfg.Sync.MaxConcurrentSyncs != 4 || cfg.AI.Stream {
		t.Fatalf("env bindings wrong: model=%s", cfg.AI.Model)
	}
	if cfg.Sources["ai.model"] != "environment" || cfg.Sources["ai.timeout_sec"] != "configuration" || cfg.Sources["sync.auto_sync_minutes"] != "configuration" {
		t.Fatalf("wrong source: %+v", cfg.Sources)
	}
	if value := BoundValues(cfg)["ai.temperature"]; value != "0.45" {
		t.Fatalf("wrong serialized number %s", value)
	}
	ApplyValues(&cfg, map[string]string{"ai.model": "admin-model", "sync.auto_sync_minutes": "2", "ai.disabled_models": "model-a, model-b"})
	if cfg.AI.Model != "admin-model" || cfg.Sync.AutoSyncMinutes != 2 || len(cfg.AI.DisabledModels) != 2 {
		t.Fatal("typed override failed")
	}
}

func TestInvalidEnvironmentNumbersDoNotPoisonDefaults(t *testing.T) {
	t.Setenv("POSTRA_AI_TEMPERATURE", "NaN")
	t.Setenv("POSTRA_MAX_CONCURRENT_SYNCS", "invalid")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.Temperature != Default().AI.Temperature || cfg.Sync.MaxConcurrentSyncs != Default().Sync.MaxConcurrentSyncs {
		t.Fatal("invalid environment accepted")
	}
	if cfg.Sources["ai.temperature"] != "" {
		t.Fatal("invalid env incorrectly reported as source")
	}
}
