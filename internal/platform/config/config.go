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

	// StorageDriver selects the persistence backend: "sqlite" (default,
	// personal/embedded) or "postgres" (server/multi-user, enables pgvector
	// semantic search at scale). PostgresDSN is required for "postgres".
	StorageDriver string `json:"storage_driver"`
	PostgresDSN   string `json:"postgres_dsn"`

	// HTTPAddr is the REST API bind address (e.g. "127.0.0.1:8480").
	HTTPAddr string `json:"http_addr"`
	// MCPHTTPAddr optionally exposes a second, legacy-compatible dedicated
	// MCP listener (e.g. "127.0.0.1:8481"). Streamable HTTP MCP is always
	// available at /mcp on HTTPAddr; empty keeps the default single-port mode.
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

	// MetricsEnabled exposes Prometheus metrics at GET /metrics on the REST
	// bind address, unauthenticated (for scraping). Default true; disable to
	// remove the endpoint entirely. Restrict exposure via HTTPAddr binding.
	MetricsEnabled bool `json:"metrics_enabled"`

	// WebUIEnabled serves the minimal server-rendered web UI (search, draft
	// review, send approval) under /ui on the REST bind address. Default true.
	// When APIToken is set the UI requires a cookie login with that token.
	WebUIEnabled bool `json:"web_ui_enabled"`

	// WorkerEnabled controls whether this node runs background tasks (sync, retry outbox, embed build).
	WorkerEnabled bool `json:"worker_enabled"`

	AI          AIConfig         `json:"ai"`
	Auth        AuthConfig       `json:"auth"`
	Sync        SyncConfig       `json:"sync"`
	Send        SendConfig       `json:"send"`
	Attachments AttachmentConfig `json:"attachments"`
}

type AuthConfig struct {
	Enabled           bool   `json:"enabled"`
	SessionHours      int    `json:"session_hours"`
	BootstrapAdmin    string `json:"bootstrap_admin"`
	BootstrapPassword string `json:"-"`
	OIDCIssuer        string `json:"oidc_issuer"`
	OIDCClientID      string `json:"oidc_client_id"`
	OIDCClientSecret  string `json:"-"`
	OIDCSecretRef     string `json:"oidc_secret_ref"`
	OIDCRedirectURL   string `json:"oidc_redirect_url"`
	OIDCAutoProvision bool   `json:"oidc_auto_provision"`
	OIDCAdminGroup    string `json:"oidc_admin_group"`
}

// AttachmentConfig drives the heuristic attachment scanner (MIME-011/012).
type AttachmentConfig struct {
	// BlockExtensions: content is NOT retained; download is refused.
	BlockExtensions []string `json:"block_extensions"`
	// QuarantineExtensions: content stored but flagged; download is gated.
	QuarantineExtensions []string `json:"quarantine_extensions"`
	// Archive (zip-bomb) limits (MIME-011).
	ArchiveMaxEntries    int     `json:"archive_max_entries"`
	ArchiveMaxTotalBytes int64   `json:"archive_max_total_bytes"`
	ArchiveMaxRatio      float64 `json:"archive_max_ratio"`
}

type SendConfig struct {
	// MaxPerMinute / MaxPerHour cap sent messages per account over a rolling
	// window (SMTP-012). 0 = unlimited.
	MaxPerMinute int `json:"max_per_minute"`
	MaxPerHour   int `json:"max_per_hour"`
	// WarnRecipients surfaces a preview warning when a single send targets at
	// least this many recipients (SMTP-013). 0 disables the warning.
	WarnRecipients int `json:"warn_recipients"`
	// Outbox retry policy for temporary SMTP failures (SMTP-010/011).
	// MaxRetries is total attempts (1 = no retry). Backoff is exponential
	// from RetryBaseSeconds, capped at RetryMaxSeconds.
	MaxRetries       int `json:"max_retries"`
	RetryBaseSeconds int `json:"retry_base_seconds"`
	RetryMaxSeconds  int `json:"retry_max_seconds"`
}

type AIConfig struct {
	// BaseURL of an OpenAI-compatible API, e.g. "http://localhost:8000/v1"
	// (vLLM, Ollama, or a hosted provider).
	BaseURL      string            `json:"base_url"`
	Model        string            `json:"model"`
	EmbedModel   string            `json:"embed_model"`
	EmbedBaseURL string            `json:"embed_base_url"`
	APIKeyRef    string            `json:"api_key_ref"`
	TimeoutSec   int               `json:"timeout_sec"`
	MaxTokens    int               `json:"max_tokens"`
	AllowExternal bool              `json:"allow_external"`
	MaskExternalPII bool            `json:"mask_external_pii"`
	PromptVersions map[string]string `json:"prompt_versions,omitempty"`
	Stream       bool              `json:"stream"`
	ExtraHeaders string            `json:"extra_headers"`
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
		StorageDriver:     "sqlite",
		HTTPAddr:          "127.0.0.1:8480",
		MCPHTTPAddr:       "",
		AllowPrivateHosts: true,
		EncryptAtRest:     true,
		MetricsEnabled:    true,
		WebUIEnabled:      true,
		WorkerEnabled:     true,
		Auth: AuthConfig{
			Enabled:           true,
			SessionHours:      12,
			BootstrapAdmin:    "admin",
			OIDCAutoProvision: true,
			OIDCAdminGroup:    "postra-admins",
		},
		AI: AIConfig{
			BaseURL:         "http://127.0.0.1:11434/v1",
			Model:           "llama3.1",
			TimeoutSec:      120,
			MaxTokens:       2048,
			MaskExternalPII: true,
		},
		Sync: SyncConfig{
			MaxMessageBytes:   50 << 20,
			MaxPerSync:        0, // 0 = unlimited / full sync
			ConnectTimeoutSec: 15,
			CommandTimeoutSec: 60,
		},

		Send: SendConfig{
			MaxPerMinute:     20,
			MaxPerHour:       200,
			WarnRecipients:   10,
			MaxRetries:       4,
			RetryBaseSeconds: 30,
			RetryMaxSeconds:  1800,
		},
		Attachments: AttachmentConfig{
			BlockExtensions: []string{
				"exe", "com", "scr", "pif", "bat", "cmd", "vbs", "vbe", "js", "jse",
				"ws", "wsf", "wsh", "ps1", "msi", "msp", "hta", "cpl", "jar", "reg",
			},
			QuarantineExtensions: []string{"docm", "xlsm", "pptm", "dll", "iso", "img", "lnk"},
			ArchiveMaxEntries:    1000,
			ArchiveMaxTotalBytes: 500 << 20, // 500 MiB uncompressed
			ArchiveMaxRatio:      100,       // uncompressed:compressed
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
	if b, err := os.ReadFile(path); err == nil { // #nosec G304 -- config path from operator flag/home dir, not untrusted input
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return cfg, err
	}
	applyEnv(&cfg)
	if cfg.PostgresDSN != "" && (cfg.StorageDriver == "" || cfg.StorageDriver == "sqlite") && os.Getenv("POSTRA_STORAGE_DRIVER") != "sqlite" {
		cfg.StorageDriver = "postgres"
	}
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
	set("POSTRA_STORAGE_DRIVER", &cfg.StorageDriver)
	set("POSTRA_POSTGRES_DSN", &cfg.PostgresDSN)
	set("POSTRA_HTTP_ADDR", &cfg.HTTPAddr)
	set("POSTRA_MCP_HTTP_ADDR", &cfg.MCPHTTPAddr)
	set("POSTRA_API_TOKEN", &cfg.APIToken)
	setBool("POSTRA_WORKER_ENABLED", &cfg.WorkerEnabled)
	setBool("POSTRA_ALLOW_INSECURE_MAIL", &cfg.AllowInsecureMail)
	setBool("POSTRA_ALLOW_PRIVATE_HOSTS", &cfg.AllowPrivateHosts)
	set("POSTRA_AI_BASE_URL", &cfg.AI.BaseURL)
	set("POSTRA_AI_MODEL", &cfg.AI.Model)
	set("POSTRA_AI_API_KEY_REF", &cfg.AI.APIKeyRef)
	setBool("POSTRA_AI_ALLOW_EXTERNAL", &cfg.AI.AllowExternal)
	setBool("POSTRA_AUTH_ENABLED", &cfg.Auth.Enabled)
	set("POSTRA_BOOTSTRAP_ADMIN", &cfg.Auth.BootstrapAdmin)
	set("POSTRA_BOOTSTRAP_ADMIN_PASSWORD", &cfg.Auth.BootstrapPassword)
	set("POSTRA_OIDC_ISSUER", &cfg.Auth.OIDCIssuer)
	set("POSTRA_OIDC_CLIENT_ID", &cfg.Auth.OIDCClientID)
	set("POSTRA_OIDC_CLIENT_SECRET", &cfg.Auth.OIDCClientSecret)
	set("POSTRA_OIDC_REDIRECT_URL", &cfg.Auth.OIDCRedirectURL)
	setBool("POSTRA_OIDC_AUTO_PROVISION", &cfg.Auth.OIDCAutoProvision)
	set("POSTRA_OIDC_ADMIN_GROUP", &cfg.Auth.OIDCAdminGroup)
}

// Save writes the config to path with restrictive permissions.
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// #nosec G117 -- config intentionally persists the API token to the operator-owned 0700 config file.
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
