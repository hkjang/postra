package application

import (
	"context"
	"strconv"
	"strings"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

const (
	SettingAuthSessionHours      = "auth.session_hours"
	SettingOIDCIssuer            = "auth.oidc.issuer"
	SettingOIDCClientID          = "auth.oidc.client_id"
	SettingOIDCSecretRef         = "auth.oidc.secret_ref"   // #nosec G101 -- setting key, never a credential value
	SettingOIDCRedirectURL       = "auth.oidc.redirect_url" // #nosec G101 -- setting key, not a credential
	SettingOIDCAutoProvision     = "auth.oidc.auto_provision"
	SettingOIDCAdminGroup        = "auth.oidc.admin_group"
	SettingSyncAutoMinutes       = "sync.auto_sync_minutes"
	SettingSyncInitialWindowDays = "sync.initial_window_days"
	SettingSyncMaxMessageBytes   = "sync.max_message_bytes"
	SettingSyncMaxPerSync        = "sync.max_per_sync"
	SettingSyncConnectTimeout    = "sync.connect_timeout_sec"
	SettingSyncCommandTimeout    = "sync.command_timeout_sec"
	SettingAIBaseURL             = "ai.base_url"
	SettingAIModel               = "ai.model"
	SettingAIEmbedModel          = "ai.embed_model"
	SettingAIAPIKeyRef           = "ai.api_key_ref" // #nosec G101 -- encrypted-secret reference setting key
	SettingAITimeout             = "ai.timeout_sec"
	SettingAIMaxTokens           = "ai.max_tokens"
	SettingAIAllowExternal       = "ai.allow_external"
	SettingAIMaskExternalPII     = "ai.mask_external_pii"
	SettingSendMaxMinute         = "send.max_per_minute"
	SettingSendMaxHour           = "send.max_per_hour"
	SettingSendWarnRecipients    = "send.warn_recipients"
	SettingSendMaxRetries        = "send.max_retries"
	SettingSendRetryBase         = "send.retry_base_seconds"
	SettingSendRetryMax          = "send.retry_max_seconds"
	SettingAttachmentBlock       = "attachments.block_extensions"
	SettingAttachmentQuarantine  = "attachments.quarantine_extensions"
	SettingArchiveMaxEntries     = "attachments.archive_max_entries"
	SettingArchiveMaxTotalBytes  = "attachments.archive_max_total_bytes"
	SettingArchiveMaxRatio       = "attachments.archive_max_ratio"
	SettingAllowInsecureMail     = "security.allow_insecure_mail"
	SettingAllowPrivateHosts     = "security.allow_private_hosts"
	SettingEncryptAtRest         = "security.encrypt_at_rest"
)

var allowedSettings = map[string]bool{
	SettingAuthSessionHours: true, SettingOIDCIssuer: true, SettingOIDCClientID: true,
	SettingOIDCSecretRef: true, SettingOIDCRedirectURL: true, SettingOIDCAutoProvision: true,
	SettingOIDCAdminGroup: true, SettingSyncAutoMinutes: true, SettingSyncInitialWindowDays: true,
	SettingSyncMaxMessageBytes: true, SettingSyncMaxPerSync: true, SettingSyncConnectTimeout: true,
	SettingSyncCommandTimeout: true, SettingAIBaseURL: true, SettingAIModel: true,
	SettingAIEmbedModel: true, SettingAIAPIKeyRef: true, SettingAITimeout: true,
	SettingAIMaxTokens: true, SettingAIAllowExternal: true, SettingAIMaskExternalPII: true,
	SettingSendMaxMinute: true, SettingSendMaxHour: true, SettingSendWarnRecipients: true,
	SettingSendMaxRetries: true, SettingSendRetryBase: true, SettingSendRetryMax: true,
	SettingAttachmentBlock: true, SettingAttachmentQuarantine: true, SettingArchiveMaxEntries: true,
	SettingArchiveMaxTotalBytes: true, SettingArchiveMaxRatio: true,
	SettingAllowInsecureMail: true, SettingAllowPrivateHosts: true, SettingEncryptAtRest: true,
}

func (a *App) SystemSettings(ctx context.Context) (map[string]string, error) {
	stored, err := a.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	defaults := map[string]string{
		SettingAuthSessionHours: strconv.Itoa(a.Cfg.Auth.SessionHours),
		SettingOIDCIssuer:       a.Cfg.Auth.OIDCIssuer, SettingOIDCClientID: a.Cfg.Auth.OIDCClientID,
		SettingOIDCSecretRef: a.Cfg.Auth.OIDCSecretRef, SettingOIDCRedirectURL: a.Cfg.Auth.OIDCRedirectURL,
		SettingOIDCAutoProvision:     strconv.FormatBool(a.Cfg.Auth.OIDCAutoProvision),
		SettingOIDCAdminGroup:        a.Cfg.Auth.OIDCAdminGroup,
		SettingSyncAutoMinutes:       strconv.Itoa(a.Cfg.Sync.AutoSyncMinutes),
		SettingSyncInitialWindowDays: strconv.Itoa(a.Cfg.Sync.InitialWindowDays),
		SettingSyncMaxMessageBytes:   strconv.FormatInt(a.Cfg.Sync.MaxMessageBytes, 10),
		SettingSyncMaxPerSync:        strconv.Itoa(a.Cfg.Sync.MaxPerSync),
		SettingSyncConnectTimeout:    strconv.Itoa(a.Cfg.Sync.ConnectTimeoutSec),
		SettingSyncCommandTimeout:    strconv.Itoa(a.Cfg.Sync.CommandTimeoutSec),
		SettingAIBaseURL:             a.Cfg.AI.BaseURL, SettingAIModel: a.Cfg.AI.Model,
		SettingAIEmbedModel:         a.Cfg.AI.EmbedModel,
		SettingAIAPIKeyRef:          a.Cfg.AI.APIKeyRef,
		SettingAITimeout:            strconv.Itoa(a.Cfg.AI.TimeoutSec),
		SettingAIMaxTokens:          strconv.Itoa(a.Cfg.AI.MaxTokens),
		SettingAIAllowExternal:      strconv.FormatBool(a.Cfg.AI.AllowExternal),
		SettingAIMaskExternalPII:    strconv.FormatBool(a.Cfg.AI.MaskExternalPII),
		SettingSendMaxMinute:        strconv.Itoa(a.Cfg.Send.MaxPerMinute),
		SettingSendMaxHour:          strconv.Itoa(a.Cfg.Send.MaxPerHour),
		SettingSendWarnRecipients:   strconv.Itoa(a.Cfg.Send.WarnRecipients),
		SettingSendMaxRetries:       strconv.Itoa(a.Cfg.Send.MaxRetries),
		SettingSendRetryBase:        strconv.Itoa(a.Cfg.Send.RetryBaseSeconds),
		SettingSendRetryMax:         strconv.Itoa(a.Cfg.Send.RetryMaxSeconds),
		SettingAttachmentBlock:      strings.Join(a.Cfg.Attachments.BlockExtensions, ","),
		SettingAttachmentQuarantine: strings.Join(a.Cfg.Attachments.QuarantineExtensions, ","),
		SettingArchiveMaxEntries:    strconv.Itoa(a.Cfg.Attachments.ArchiveMaxEntries),
		SettingArchiveMaxTotalBytes: strconv.FormatInt(a.Cfg.Attachments.ArchiveMaxTotalBytes, 10),
		SettingArchiveMaxRatio:      strconv.FormatFloat(a.Cfg.Attachments.ArchiveMaxRatio, 'f', -1, 64),
		SettingAllowInsecureMail:    strconv.FormatBool(a.Cfg.AllowInsecureMail),
		SettingAllowPrivateHosts:    strconv.FormatBool(a.Cfg.AllowPrivateHosts),
		SettingEncryptAtRest:        strconv.FormatBool(a.Cfg.EncryptAtRest),
	}
	for key, value := range defaults {
		if _, ok := stored[key]; !ok {
			stored[key] = value
		}
	}
	return stored, nil
}

// applyStoredSettings applies administrator-managed policy before application
// services and scanners are constructed. Authentication/OIDC settings are also
// read dynamically, while other policy changes take effect after restart.
func applyStoredSettings(cfg *config.Config, values map[string]string) {
	cfg.Auth.SessionHours = intSetting(values, SettingAuthSessionHours, cfg.Auth.SessionHours)
	cfg.Sync.AutoSyncMinutes = intSetting(values, SettingSyncAutoMinutes, cfg.Sync.AutoSyncMinutes)
	cfg.Sync.InitialWindowDays = intSetting(values, SettingSyncInitialWindowDays, cfg.Sync.InitialWindowDays)
	cfg.Sync.MaxMessageBytes = int64Setting(values, SettingSyncMaxMessageBytes, cfg.Sync.MaxMessageBytes)
	cfg.Sync.MaxPerSync = intSetting(values, SettingSyncMaxPerSync, cfg.Sync.MaxPerSync)
	cfg.Sync.ConnectTimeoutSec = intSetting(values, SettingSyncConnectTimeout, cfg.Sync.ConnectTimeoutSec)
	cfg.Sync.CommandTimeoutSec = intSetting(values, SettingSyncCommandTimeout, cfg.Sync.CommandTimeoutSec)
	cfg.AI.BaseURL = stringSetting(values, SettingAIBaseURL, cfg.AI.BaseURL)
	cfg.AI.Model = stringSetting(values, SettingAIModel, cfg.AI.Model)
	cfg.AI.EmbedModel = stringSetting(values, SettingAIEmbedModel, cfg.AI.EmbedModel)
	cfg.AI.APIKeyRef = stringSetting(values, SettingAIAPIKeyRef, cfg.AI.APIKeyRef)
	cfg.AI.TimeoutSec = intSetting(values, SettingAITimeout, cfg.AI.TimeoutSec)
	cfg.AI.MaxTokens = intSetting(values, SettingAIMaxTokens, cfg.AI.MaxTokens)
	cfg.AI.AllowExternal = boolSetting(values, SettingAIAllowExternal, cfg.AI.AllowExternal)
	cfg.AI.MaskExternalPII = boolSetting(values, SettingAIMaskExternalPII, cfg.AI.MaskExternalPII)
	cfg.Send.MaxPerMinute = intSetting(values, SettingSendMaxMinute, cfg.Send.MaxPerMinute)
	cfg.Send.MaxPerHour = intSetting(values, SettingSendMaxHour, cfg.Send.MaxPerHour)
	cfg.Send.WarnRecipients = intSetting(values, SettingSendWarnRecipients, cfg.Send.WarnRecipients)
	cfg.Send.MaxRetries = intSetting(values, SettingSendMaxRetries, cfg.Send.MaxRetries)
	cfg.Send.RetryBaseSeconds = intSetting(values, SettingSendRetryBase, cfg.Send.RetryBaseSeconds)
	cfg.Send.RetryMaxSeconds = intSetting(values, SettingSendRetryMax, cfg.Send.RetryMaxSeconds)
	cfg.Attachments.BlockExtensions = csvSetting(values, SettingAttachmentBlock, cfg.Attachments.BlockExtensions)
	cfg.Attachments.QuarantineExtensions = csvSetting(values, SettingAttachmentQuarantine, cfg.Attachments.QuarantineExtensions)
	cfg.Attachments.ArchiveMaxEntries = intSetting(values, SettingArchiveMaxEntries, cfg.Attachments.ArchiveMaxEntries)
	cfg.Attachments.ArchiveMaxTotalBytes = int64Setting(values, SettingArchiveMaxTotalBytes, cfg.Attachments.ArchiveMaxTotalBytes)
	cfg.Attachments.ArchiveMaxRatio = floatSetting(values, SettingArchiveMaxRatio, cfg.Attachments.ArchiveMaxRatio)
	cfg.AllowInsecureMail = boolSetting(values, SettingAllowInsecureMail, cfg.AllowInsecureMail)
	cfg.AllowPrivateHosts = boolSetting(values, SettingAllowPrivateHosts, cfg.AllowPrivateHosts)
	cfg.EncryptAtRest = boolSetting(values, SettingEncryptAtRest, cfg.EncryptAtRest)
}

func (a *App) AdminSaveSettings(ctx context.Context, values map[string]string, oidcClientSecret string) error {
	if _, err := requireAdmin(ctx); err != nil {
		return err
	}
	clean := map[string]string{}
	for key, value := range values {
		if allowedSettings[key] {
			clean[key] = strings.TrimSpace(value)
		}
	}
	if redirect := clean[SettingOIDCRedirectURL]; redirect != "" {
		if err := ValidateOIDCRedirect(redirect); err != nil {
			return err
		}
	}
	if issuer := clean[SettingOIDCIssuer]; issuer != "" {
		if err := ValidateOIDCRedirect(issuer); err != nil {
			return userErrf("OIDC issuer: %v", err)
		}
	}
	if oidcClientSecret != "" {
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "OIDC client secret",
			domain.NewSecretHandle([]byte(oidcClientSecret)))
		if err != nil {
			return err
		}
		clean[SettingOIDCSecretRef] = string(ref)
	}
	if err := a.Store.UpsertSettings(ctx, clean); err != nil {
		return err
	}
	a.audit(ctx, "settings_update", "system", "ok", "keys="+strconv.Itoa(len(clean)))
	return nil
}

func boolSetting(values map[string]string, key string, fallback bool) bool {
	if value, ok := values[key]; ok {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func intSetting(values map[string]string, key string, fallback int) int {
	if value, ok := values[key]; ok {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func int64Setting(values map[string]string, key string, fallback int64) int64 {
	if parsed, err := strconv.ParseInt(values[key], 10, 64); err == nil {
		return parsed
	}
	return fallback
}

func floatSetting(values map[string]string, key string, fallback float64) float64 {
	if parsed, err := strconv.ParseFloat(values[key], 64); err == nil {
		return parsed
	}
	return fallback
}

func stringSetting(values map[string]string, key, fallback string) string {
	if value, ok := values[key]; ok {
		return value
	}
	return fallback
}

func csvSetting(values map[string]string, key string, fallback []string) []string {
	value, ok := values[key]
	if !ok {
		return fallback
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(strings.TrimPrefix(item, ".")); item != "" {
			out = append(out, strings.ToLower(item))
		}
	}
	return out
}
