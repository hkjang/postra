// Package-level bootstrap bindings keep environment reads in one place.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
)

type Binding struct{ Key, Path, Env string }

var Bindings = []Binding{
	{"system.http_addr", "HTTPAddr", "POSTRA_HTTP_ADDR"},
	{"system.mcp_http_addr", "MCPHTTPAddr", "POSTRA_MCP_HTTP_ADDR"},
	{"storage.data_dir", "DataDir", "POSTRA_DATA_DIR"},
	{"storage.driver", "StorageDriver", "POSTRA_STORAGE_DRIVER"},
	{"storage.postgres_dsn", "PostgresDSN", "POSTRA_POSTGRES_DSN"},
	{"security.api_token", "APIToken", "POSTRA_API_TOKEN"},
	{"security.allow_insecure_mail", "AllowInsecureMail", "POSTRA_ALLOW_INSECURE_MAIL"},
	{"security.allow_private_hosts", "AllowPrivateHosts", "POSTRA_ALLOW_PRIVATE_HOSTS"},
	{"security.encrypt_at_rest", "EncryptAtRest", "POSTRA_ENCRYPT_AT_REST"},
	{"system.metrics_enabled", "MetricsEnabled", "POSTRA_METRICS_ENABLED"},
	{"system.web_ui_enabled", "WebUIEnabled", "POSTRA_WEB_UI_ENABLED"},
	{"system.worker_enabled", "WorkerEnabled", "POSTRA_WORKER_ENABLED"},
	{"system.telemetry_enabled", "TelemetryEnabled", "POSTRA_TELEMETRY_ENABLED"},
	{"auth.enabled", "Auth.Enabled", "POSTRA_AUTH_ENABLED"},
	{"auth.session_hours", "Auth.SessionHours", "POSTRA_SESSION_HOURS"},
	{"auth.bootstrap_admin", "Auth.BootstrapAdmin", "POSTRA_BOOTSTRAP_ADMIN"},
	{"auth.bootstrap_password", "Auth.BootstrapPassword", "POSTRA_BOOTSTRAP_ADMIN_PASSWORD"},
	{"auth.oidc.issuer", "Auth.OIDCIssuer", "POSTRA_OIDC_ISSUER"},
	{"auth.oidc.client_id", "Auth.OIDCClientID", "POSTRA_OIDC_CLIENT_ID"},
	{"auth.oidc.client_secret", "Auth.OIDCClientSecret", "POSTRA_OIDC_CLIENT_SECRET"},
	{"auth.oidc.secret_ref", "Auth.OIDCSecretRef", "POSTRA_OIDC_SECRET_REF"},
	{"auth.oidc.redirect_url", "Auth.OIDCRedirectURL", "POSTRA_OIDC_REDIRECT_URL"},
	{"auth.oidc.auto_provision", "Auth.OIDCAutoProvision", "POSTRA_OIDC_AUTO_PROVISION"},
	{"auth.oidc.auto_login", "Auth.OIDCAutoLogin", "POSTRA_OIDC_AUTO_LOGIN"},
	{"auth.oidc.admin_group", "Auth.OIDCAdminGroup", "POSTRA_OIDC_ADMIN_GROUP"},
	{"ai.base_url", "AI.BaseURL", "POSTRA_AI_BASE_URL"},
	{"ai.model", "AI.Model", "POSTRA_AI_MODEL"},
	{"ai.embed_model", "AI.EmbedModel", "POSTRA_AI_EMBED_MODEL"},
	{"ai.embed_base_url", "AI.EmbedBaseURL", "POSTRA_AI_EMBED_BASE_URL"},
	{"ai.api_key_ref", "AI.APIKeyRef", "POSTRA_AI_API_KEY_REF"},
	{"ai.timeout_sec", "AI.TimeoutSec", "POSTRA_AI_TIMEOUT_SEC"},
	{"ai.max_tokens", "AI.MaxTokens", "POSTRA_AI_MAX_TOKENS"},
	{"ai.context_length", "AI.ContextLength", "POSTRA_AI_CONTEXT_LENGTH"},
	{"ai.auto_context_length", "AI.AutoContextLength", "POSTRA_AI_AUTO_CONTEXT_LENGTH"},
	{"ai.temperature", "AI.Temperature", "POSTRA_AI_TEMPERATURE"},
	{"ai.disabled_models", "AI.DisabledModels", "POSTRA_AI_DISABLED_MODELS"},
	{"ai.allow_external", "AI.AllowExternal", "POSTRA_AI_ALLOW_EXTERNAL"},
	{"ai.mask_external_pii", "AI.MaskExternalPII", "POSTRA_AI_MASK_EXTERNAL_PII"},
	{"ai.stream", "AI.Stream", "POSTRA_AI_STREAM"},
	{"ai.extra_headers", "AI.ExtraHeaders", "POSTRA_AI_EXTRA_HEADERS"},
	{"ai.extra_headers_ref", "AI.ExtraHeadersRef", "POSTRA_AI_EXTRA_HEADERS_REF"},
	{"ai.task_models", "AI.TaskModels", "POSTRA_AI_TASK_MODELS"},
	{"ai.cost_per_1m_input_tokens", "AI.CostPer1MInputTokens", "POSTRA_AI_COST_PER_1M_INPUT_TOKENS"},
	{"ai.cost_per_1m_output_tokens", "AI.CostPer1MOutputTokens", "POSTRA_AI_COST_PER_1M_OUTPUT_TOKENS"},
	{"sync.auto_sync_minutes", "Sync.AutoSyncMinutes", "POSTRA_AUTO_SYNC_MINUTES"},
	{"sync.initial_window_days", "Sync.InitialWindowDays", "POSTRA_SYNC_INITIAL_WINDOW_DAYS"},
	{"sync.max_message_bytes", "Sync.MaxMessageBytes", "POSTRA_SYNC_MAX_MESSAGE_BYTES"},
	{"sync.max_per_sync", "Sync.MaxPerSync", "POSTRA_SYNC_MAX_PER_SYNC"},
	{"sync.connect_timeout_sec", "Sync.ConnectTimeoutSec", "POSTRA_SYNC_CONNECT_TIMEOUT_SEC"},
	{"sync.command_timeout_sec", "Sync.CommandTimeoutSec", "POSTRA_SYNC_COMMAND_TIMEOUT_SEC"},
	{"sync.auto_embed_minutes", "Sync.AutoEmbedMinutes", "POSTRA_AUTO_EMBED_MINUTES"},
	{"sync.auto_triage", "Sync.AutoTriage", "POSTRA_AUTO_TRIAGE"},
	{"sync.auto_triage_minutes", "Sync.AutoTriageMinutes", "POSTRA_AUTO_TRIAGE_MINUTES"},
	{"sync.daily_digest_enabled", "Sync.DailyDigestEnabled", "POSTRA_DAILY_DIGEST_ENABLED"},
	{"sync.daily_digest_hour", "Sync.DailyDigestHour", "POSTRA_DAILY_DIGEST_HOUR"},
	{"sync.max_concurrent_syncs", "Sync.MaxConcurrentSyncs", "POSTRA_MAX_CONCURRENT_SYNCS"},
	{"send.max_per_minute", "Send.MaxPerMinute", "POSTRA_SEND_MAX_PER_MINUTE"},
	{"send.max_per_hour", "Send.MaxPerHour", "POSTRA_SEND_MAX_PER_HOUR"},
	{"send.warn_recipients", "Send.WarnRecipients", "POSTRA_SEND_WARN_RECIPIENTS"},
	{"send.max_retries", "Send.MaxRetries", "POSTRA_SEND_MAX_RETRIES"},
	{"send.retry_base_seconds", "Send.RetryBaseSeconds", "POSTRA_SEND_RETRY_BASE_SECONDS"},
	{"send.retry_max_seconds", "Send.RetryMaxSeconds", "POSTRA_SEND_RETRY_MAX_SECONDS"},
	{"send.dlp_policy", "Send.DLPPolicy", "POSTRA_SEND_DLP_POLICY"},
	{"send.dlp_keywords", "Send.DLPKeywords", "POSTRA_SEND_DLP_KEYWORDS"},
	{"compose.writing_guide", "Compose.WritingGuide", "POSTRA_COMPOSE_WRITING_GUIDE"},
	{"compose.banned_phrases", "Compose.BannedPhrases", "POSTRA_COMPOSE_BANNED_PHRASES"},
	{"attachments.block_extensions", "Attachments.BlockExtensions", "POSTRA_ATTACHMENT_BLOCK_EXTENSIONS"},
	{"attachments.quarantine_extensions", "Attachments.QuarantineExtensions", "POSTRA_ATTACHMENT_QUARANTINE_EXTENSIONS"},
	{"attachments.archive_max_entries", "Attachments.ArchiveMaxEntries", "POSTRA_ARCHIVE_MAX_ENTRIES"},
	{"attachments.archive_max_total_bytes", "Attachments.ArchiveMaxTotalBytes", "POSTRA_ARCHIVE_MAX_TOTAL_BYTES"},
	{"attachments.archive_max_ratio", "Attachments.ArchiveMaxRatio", "POSTRA_ARCHIVE_MAX_RATIO"},
}

func configField(c *Config, path string) reflect.Value {
	v := reflect.ValueOf(c).Elem()
	for _, part := range strings.Split(path, ".") {
		v = v.FieldByName(part)
		if !v.IsValid() {
			return v
		}
	}
	return v
}

func BoundValues(c Config) map[string]string {
	out := map[string]string{}
	for _, b := range Bindings {
		v := configField(&c, b.Path)
		switch v.Kind() {
		case reflect.String:
			out[b.Key] = v.String()
		case reflect.Bool:
			out[b.Key] = strconv.FormatBool(v.Bool())
		case reflect.Int, reflect.Int64:
			out[b.Key] = strconv.FormatInt(v.Int(), 10)
		case reflect.Float64:
			out[b.Key] = strconv.FormatFloat(v.Float(), 'f', -1, 64)
		case reflect.Slice:
			if v.Type().Elem().Kind() == reflect.String {
				items := make([]string, v.Len())
				for i := range items {
					items[i] = v.Index(i).String()
				}
				out[b.Key] = strings.Join(items, ",")
			}
		default:
			raw, _ := json.Marshal(v.Interface())
			if string(raw) != "null" {
				out[b.Key] = string(raw)
			}
		}
	}
	return out
}

func setBoundValue(c *Config, b Binding, s string) error {
	v := configField(c, b.Path)
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		value, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(value)
	case reflect.Int, reflect.Int64:
		value, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(value)
	case reflect.Float64:
		value, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			return fmt.Errorf("invalid number")
		}
		v.SetFloat(value)
	case reflect.Slice:
		var items []string
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				items = append(items, part)
			}
		}
		v.Set(reflect.ValueOf(items))
	default:
		ptr := reflect.New(v.Type())
		if strings.TrimSpace(s) == "" {
			s = "null"
		}
		if err := json.Unmarshal([]byte(s), ptr.Interface()); err != nil {
			return err
		}
		v.Set(ptr.Elem())
	}
	return nil
}

// ApplyValues only accepts known typed bindings; transport-facing validation
// and range/policy enforcement belong to the application settings service.
func ApplyValues(c *Config, values map[string]string) {
	for _, b := range Bindings {
		if s, ok := values[b.Key]; ok {
			_ = setBoundValue(c, b, s)
		}
	}
}

func applyBoundEnvironment(c *Config) {
	if c.Sources == nil {
		c.Sources = map[string]string{}
	}
	for _, b := range Bindings {
		if value, ok := os.LookupEnv(b.Env); ok && value != "" {
			if setBoundValue(c, b, value) == nil {
				c.Sources[b.Key] = "environment"
			}
		}
	}
}

func recordFileSources(c *Config, raw []byte) {
	if c.Sources == nil {
		c.Sources = map[string]string{}
	}
	var file map[string]any
	if json.Unmarshal(raw, &file) != nil {
		return
	}
	for _, b := range Bindings {
		var object any = file
		t := reflect.TypeOf(*c)
		found := true
		for _, part := range strings.Split(b.Path, ".") {
			field, ok := t.FieldByName(part)
			if !ok {
				found = false
				break
			}
			key := strings.Split(field.Tag.Get("json"), ",")[0]
			m, ok := object.(map[string]any)
			if !ok {
				found = false
				break
			}
			object, ok = m[key]
			if !ok {
				found = false
				break
			}
			t = field.Type
		}
		if found {
			c.Sources[b.Key] = "configuration"
		}
	}
}
