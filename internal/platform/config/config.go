// Package config loads Postra configuration from a JSON file with
// environment-variable overrides. Secrets are never stored here; only
// secret references are allowed in configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	// DataDir holds the SQLite DB, object store, and local secret store.
	DataDir string `json:"data_dir"`

	// HTTPAddr is the REST API bind address (e.g. "127.0.0.1:8480").
	HTTPAddr string `json:"http_addr"`
	// MCPHTTPAddr is the Streamable HTTP MCP bind address (e.g. "127.0.0.1:8481").
	// Empty disables the remote MCP endpoint.
	MCPHTTPAddr string `json:"mcp_http_addr"`

	// APIToken, when set, is required as "Authorization: Bearer <token>" on
	// both the REST API and the remote MCP endpoint. When empty, only
	// loopback binds are accepted unless AllowInsecureMail is enabled
	// (offline / air-gapped deployments).
	APIToken string `json:"api_token"`

	// AllowInsecureMail permits mail accounts configured with
	// security "none" (plaintext POP3/SMTP) and SMTP without AUTH.
	// Intended for offline / air-gapped internal networks.
	AllowInsecureMail bool `json:"allow_insecure_mail"`

	// AllowPrivateHosts permits mail server hosts resolving to private or
	// loopback addresses (required for on-premises deployments).
	// Link-local/metadata ranges (169.254.0.0/16) are always rejected.
	AllowPrivateHosts bool `json:"allow_private_hosts"`

	// EncryptAtRest encrypts raw MIME originals, attachments, and parsed
	// message bodies with the KEK. Metadata (subjects, addresses, dates)
	// stays queryable in plaintext; the FTS index also holds body plaintext.
	EncryptAtRest bool `json:"encrypt_at_rest"`

	AI   AIConfig   `json:"ai"`
	Sync SyncConfig `json:"sync"`
	Send SendConfig `json:"send"`
}

type SendConfig struct {
	// MaxPerMinute / MaxPerHour cap sent messages per account over a rolling
	// window (SMTP-012). 0 = unlimited.
	MaxPerMinute int `json:"max_per_minute"`
	MaxPerHour   int `json:"max_per_hour"`
	// WarnRecipients surfaces a preview warning when a single send targets at
	// least this many recipients (SMTP-013). 0 disables the warning.
	WarnRecipients int `json:"warn_recipients"`
}

type AIConfig struct {
	// BaseURL of an OpenAI-compatible API, e.g. "http://localhost:8000/v1"
	// (vLLM, Ollama, or a hosted provider).
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	// EmbedModel is reserved for semantic search (post-MVP).
	EmbedModel string `json:"embed_model"`
	// APIKeyRef is a secret reference resolved through the SecretStore.
	// Never put a raw API key in configuration.
	APIKeyRef  string `json:"api_key_ref"`
	TimeoutSec int    `json:"timeout_sec"`
	MaxTokens  int    `json:"max_tokens"`
	// AllowExternal gates sending mail content to non-loopback AI endpoints.
	AllowExternal bool `json:"allow_external"`
}

type SyncConfig struct {
	MaxMessageBytes int64 `json:"max_message_bytes"`
	MaxPerSync      int   `json:"max_per_sync"`
	// InitialWindowDays limits the first sync to messages newer than N days
	// (0 = fetch everything).
	InitialWindowDays int `json:"initial_window_days"`
	ConnectTimeoutSec int `json:"connect_timeout_sec"`
	CommandTimeoutSec int `json:"command_timeout_sec"`
	// AutoSyncMinutes, when > 0, enables the background scheduler to sync all
	// active POP3 accounts on this cadence (POP-001 주기 동기화). It also acts
	// as the per-account minimum interval (POP-002). 0 disables auto-sync.
	AutoSyncMinutes int `json:"auto_sync_minutes"`
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DataDir:           filepath.Join(home, ".postra"),
		HTTPAddr:          "127.0.0.1:8480",
		MCPHTTPAddr:       "127.0.0.1:8481",
		AllowPrivateHosts: true,
		EncryptAtRest:     true,
		AI: AIConfig{
			BaseURL:    "http://127.0.0.1:11434/v1",
			Model:      "llama3.1",
			TimeoutSec: 120,
			MaxTokens:  2048,
		},
		Sync: SyncConfig{
			MaxMessageBytes:   50 << 20,
			MaxPerSync:        500,
			ConnectTimeoutSec: 15,
			CommandTimeoutSec: 60,
		},
		Send: SendConfig{
			MaxPerMinute:   20,
			MaxPerHour:     200,
			WarnRecipients: 10,
		},
	}
}

// Load reads the config file (if it exists) and applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".postra", "config.json")
	}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return cfg, err
	}
	applyEnv(&cfg)
	return cfg, nil
}

func applyEnv(cfg *Config) {
	set := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	setBool := func(key string, dst *bool) {
		if v := os.Getenv(key); v != "" {
			b, err := strconv.ParseBool(v)
			if err == nil {
				*dst = b
			}
		}
	}
	set("POSTRA_DATA_DIR", &cfg.DataDir)
	set("POSTRA_HTTP_ADDR", &cfg.HTTPAddr)
	set("POSTRA_MCP_HTTP_ADDR", &cfg.MCPHTTPAddr)
	set("POSTRA_API_TOKEN", &cfg.APIToken)
	setBool("POSTRA_ALLOW_INSECURE_MAIL", &cfg.AllowInsecureMail)
	setBool("POSTRA_ALLOW_PRIVATE_HOSTS", &cfg.AllowPrivateHosts)
	set("POSTRA_AI_BASE_URL", &cfg.AI.BaseURL)
	set("POSTRA_AI_MODEL", &cfg.AI.Model)
	set("POSTRA_AI_API_KEY_REF", &cfg.AI.APIKeyRef)
	setBool("POSTRA_AI_ALLOW_EXTERNAL", &cfg.AI.AllowExternal)
}

// Save writes the config to path with restrictive permissions.
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
