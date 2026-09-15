package application

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/config"
	"postra/internal/platform/notifymail"
	"postra/internal/platform/tracking"
)

const (
	SettingAuthSessionHours       = "auth.session_hours"
	SettingOIDCIssuer             = "auth.oidc.issuer"
	SettingOIDCClientID           = "auth.oidc.client_id"
	SettingOIDCSecretRef          = "auth.oidc.secret_ref"   // #nosec G101 -- setting key, never a credential value
	SettingOIDCRedirectURL        = "auth.oidc.redirect_url" // #nosec G101 -- setting key, not a credential
	SettingOIDCAutoProvision      = "auth.oidc.auto_provision"
	SettingOIDCAdminGroup         = "auth.oidc.admin_group"
	SettingOIDCAutoLogin          = "auth.oidc.auto_login" // silent prompt=none sign-in; off by default
	SettingSyncAutoMinutes        = "sync.auto_sync_minutes"
	SettingSyncInitialWindowDays  = "sync.initial_window_days"
	SettingSyncMaxMessageBytes    = "sync.max_message_bytes"
	SettingSyncMaxPerSync         = "sync.max_per_sync"
	SettingSyncConnectTimeout     = "sync.connect_timeout_sec"
	SettingSyncCommandTimeout     = "sync.command_timeout_sec"
	SettingAIBaseURL              = "ai.base_url"
	SettingAIModel                = "ai.model"
	SettingAIEmbedModel           = "ai.embed_model"
	SettingAIAPIKeyRef            = "ai.api_key_ref" // #nosec G101 -- encrypted-secret reference setting key
	SettingAITimeout              = "ai.timeout_sec"
	SettingAIMaxTokens            = "ai.max_tokens"
	SettingAIAllowExternal        = "ai.allow_external"
	SettingAIMaskExternalPII      = "ai.mask_external_pii"
	SettingAIStream               = "ai.stream"
	SettingAIExtraHeaders         = "ai.extra_headers"
	SettingAIEmbedBaseURL         = "ai.embed_base_url"
	SettingAITaskModels           = "ai.task_models" // JSON: {"summarize":{"model":..,"base_url":..,"api_key_ref":..,"max_tokens":..}}
	SettingSendMaxMinute          = "send.max_per_minute"
	SettingSendMaxHour            = "send.max_per_hour"
	SettingSendWarnRecipients     = "send.warn_recipients"
	SettingSendMaxRetries         = "send.max_retries"
	SettingSendRetryBase          = "send.retry_base_seconds"
	SettingSendRetryMax           = "send.retry_max_seconds"
	SettingSendDLPPolicy          = "send.dlp_policy"
	SettingSendDLPKeywords        = "send.dlp_keywords"
	SettingComposeWritingGuide    = "compose.writing_guide"
	SettingComposeBannedPhrases   = "compose.banned_phrases"
	SettingAttachmentBlock        = "attachments.block_extensions"
	SettingAttachmentQuarantine   = "attachments.quarantine_extensions"
	SettingArchiveMaxEntries      = "attachments.archive_max_entries"
	SettingArchiveMaxTotalBytes   = "attachments.archive_max_total_bytes"
	SettingArchiveMaxRatio        = "attachments.archive_max_ratio"
	SettingAllowInsecureMail      = "security.allow_insecure_mail"
	SettingAllowPrivateHosts      = "security.allow_private_hosts"
	SettingEncryptAtRest          = "security.encrypt_at_rest"
	SettingVectorProvider         = "vector.provider"
	SettingVectorMilvusURL        = "vector.milvus_url"
	SettingVectorMilvusToken      = "vector.milvus_token"     // #nosec G101 -- legacy write-only input field, never persisted
	SettingVectorMilvusTokenRef   = "vector.milvus_token_ref" // #nosec G101 -- encrypted-secret reference setting key
	SettingVectorMilvusCollection = "vector.milvus_collection"
	SettingMCPPolicy              = "mcp.policy" // JSON gateway policy for MCP tools
)

var allowedSettings = map[string]bool{
	SettingAuthSessionHours: true, SettingOIDCIssuer: true, SettingOIDCClientID: true,
	SettingOIDCSecretRef: true, SettingOIDCRedirectURL: true, SettingOIDCAutoProvision: true,
	SettingOIDCAdminGroup: true, SettingOIDCAutoLogin: true, SettingSyncAutoMinutes: true, SettingSyncInitialWindowDays: true,
	SettingSyncMaxMessageBytes: true, SettingSyncMaxPerSync: true, SettingSyncConnectTimeout: true,
	SettingSyncCommandTimeout: true, SettingAIBaseURL: true, SettingAIModel: true,
	SettingAIEmbedModel: true, SettingAIAPIKeyRef: true, SettingAITimeout: true,
	SettingAIMaxTokens: true, SettingAIAllowExternal: true, SettingAIMaskExternalPII: true,
	SettingAIStream: true, SettingAIExtraHeaders: true, SettingAIEmbedBaseURL: true,
	SettingAITaskModels:  true,
	SettingSendMaxMinute: true, SettingSendMaxHour: true, SettingSendWarnRecipients: true,
	SettingSendMaxRetries: true, SettingSendRetryBase: true, SettingSendRetryMax: true,
	SettingSendDLPPolicy: true, SettingSendDLPKeywords: true,
	SettingComposeWritingGuide: true, SettingComposeBannedPhrases: true,
	SettingAttachmentBlock: true, SettingAttachmentQuarantine: true, SettingArchiveMaxEntries: true,
	SettingArchiveMaxTotalBytes: true, SettingArchiveMaxRatio: true,
	SettingAllowInsecureMail: true, SettingAllowPrivateHosts: true, SettingEncryptAtRest: true,
	SettingVectorProvider: true, SettingVectorMilvusURL: true, SettingVectorMilvusToken: true,
	SettingVectorMilvusTokenRef: true, SettingVectorMilvusCollection: true,
	SettingMCPPolicy: true,
}

func init() {
	// Visitor tracking keys live in the tracking package so the snippet and
	// policy code never drifts from what the store accepts.
	for _, key := range tracking.SettingKeys {
		allowedSettings[key] = true
	}
}

func (a *App) SystemSettings(ctx context.Context) (map[string]string, error) {
	stored, err := a.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	for key := range stored {
		if strings.HasPrefix(key, "internal.") {
			delete(stored, key)
		}
	}
	// The Milvus token is a write-only field: never echo the plaintext (legacy
	// deployments may still have one persisted). Only the secret reference is
	// exposed, like ai.api_key_ref / auth.oidc.secret_ref.
	delete(stored, SettingVectorMilvusToken)
	aiCfg := a.currentAIConfig()
	defaults := map[string]string{
		SettingAuthSessionHours: strconv.Itoa(a.Cfg.Auth.SessionHours),
		SettingOIDCIssuer:       a.Cfg.Auth.OIDCIssuer, SettingOIDCClientID: a.Cfg.Auth.OIDCClientID,
		SettingOIDCSecretRef: a.Cfg.Auth.OIDCSecretRef, SettingOIDCRedirectURL: a.Cfg.Auth.OIDCRedirectURL,
		SettingOIDCAutoProvision:     strconv.FormatBool(a.Cfg.Auth.OIDCAutoProvision),
		SettingOIDCAdminGroup:        a.Cfg.Auth.OIDCAdminGroup,
		SettingOIDCAutoLogin:         strconv.FormatBool(a.Cfg.Auth.OIDCAutoLogin),
		SettingSyncAutoMinutes:       strconv.Itoa(a.Cfg.Sync.AutoSyncMinutes),
		SettingSyncInitialWindowDays: strconv.Itoa(a.Cfg.Sync.InitialWindowDays),
		SettingSyncMaxMessageBytes:   strconv.FormatInt(a.Cfg.Sync.MaxMessageBytes, 10),
		SettingSyncMaxPerSync:        strconv.Itoa(a.Cfg.Sync.MaxPerSync),
		SettingSyncConnectTimeout:    strconv.Itoa(a.Cfg.Sync.ConnectTimeoutSec),
		SettingSyncCommandTimeout:    strconv.Itoa(a.Cfg.Sync.CommandTimeoutSec),
		SettingAIBaseURL:             aiCfg.BaseURL, SettingAIModel: aiCfg.Model,
		SettingAIEmbedModel:           aiCfg.EmbedModel,
		SettingAIAPIKeyRef:            aiCfg.APIKeyRef,
		SettingAITimeout:              strconv.Itoa(aiCfg.TimeoutSec),
		SettingAIMaxTokens:            strconv.Itoa(aiCfg.MaxTokens),
		SettingAIAllowExternal:        strconv.FormatBool(aiCfg.AllowExternal),
		SettingAIMaskExternalPII:      strconv.FormatBool(aiCfg.MaskExternalPII),
		SettingAIStream:               strconv.FormatBool(aiCfg.Stream),
		SettingAIExtraHeaders:         aiCfg.ExtraHeaders,
		SettingAIEmbedBaseURL:         aiCfg.EmbedBaseURL,
		SettingAITaskModels:           taskModelsJSON(aiCfg.TaskModels),
		SettingSendMaxMinute:          strconv.Itoa(a.Cfg.Send.MaxPerMinute),
		SettingSendMaxHour:            strconv.Itoa(a.Cfg.Send.MaxPerHour),
		SettingSendWarnRecipients:     strconv.Itoa(a.Cfg.Send.WarnRecipients),
		SettingSendMaxRetries:         strconv.Itoa(a.Cfg.Send.MaxRetries),
		SettingSendRetryBase:          strconv.Itoa(a.Cfg.Send.RetryBaseSeconds),
		SettingSendRetryMax:           strconv.Itoa(a.Cfg.Send.RetryMaxSeconds),
		SettingSendDLPPolicy:          a.Cfg.Send.DLPPolicy,
		SettingSendDLPKeywords:        strings.Join(a.Cfg.Send.DLPKeywords, ","),
		SettingComposeWritingGuide:    a.Cfg.Compose.WritingGuide,
		SettingComposeBannedPhrases:   strings.Join(a.Cfg.Compose.BannedPhrases, ","),
		SettingAttachmentBlock:        strings.Join(a.Cfg.Attachments.BlockExtensions, ","),
		SettingAttachmentQuarantine:   strings.Join(a.Cfg.Attachments.QuarantineExtensions, ","),
		SettingArchiveMaxEntries:      strconv.Itoa(a.Cfg.Attachments.ArchiveMaxEntries),
		SettingArchiveMaxTotalBytes:   strconv.FormatInt(a.Cfg.Attachments.ArchiveMaxTotalBytes, 10),
		SettingArchiveMaxRatio:        strconv.FormatFloat(a.Cfg.Attachments.ArchiveMaxRatio, 'f', -1, 64),
		SettingAllowInsecureMail:      strconv.FormatBool(a.Cfg.AllowInsecureMail),
		SettingAllowPrivateHosts:      strconv.FormatBool(a.Cfg.AllowPrivateHosts),
		SettingEncryptAtRest:          strconv.FormatBool(a.Cfg.EncryptAtRest),
		SettingVectorProvider:         "",
		SettingVectorMilvusURL:        "",
		SettingVectorMilvusTokenRef:   "",
		SettingVectorMilvusCollection: "postra_emails",
		SettingMCPPolicy:              "",
	}
	for key, value := range tracking.Defaults {
		defaults[key] = value
	}
	for key, value := range defaults {
		if _, ok := stored[key]; !ok {
			stored[key] = value
		}
	}
	for _, d := range SettingsDefinitions() {
		if d.Apply == "fixed" {
			stored[d.Key] = d.Default
		}
	}
	// Extra headers can carry gateway credentials. Legacy APIs may report
	// registration, but must never disclose the plaintext JSON or env value.
	stored[SettingAIExtraHeaders] = ""
	stored["ai.extra_headers_registered"] = strconv.FormatBool(aiCfg.ExtraHeaders != "" || aiCfg.ExtraHeadersRef != "")
	return stored, nil
}

// applyStoredSettings applies administrator-managed policy before application
// services and scanners are constructed. Authentication/OIDC settings are also
// read dynamically, while other policy changes take effect after restart.
func applyStoredSettings(cfg *config.Config, values map[string]string) {
	// Storage/encryption/bootstrap values describe adapters already constructed
	// by the launcher. A historical DB row must not misrepresent those adapters
	// or silently toggle encryption without a data migration.
	filtered := make(map[string]string, len(values))
	for key, value := range values {
		if d, ok := settingDefinition(key); ok && (d.Apply == "deployment" || d.Apply == "fixed") {
			continue
		}
		filtered[key] = value
	}
	values = filtered
	config.ApplyValues(cfg, values)
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
	cfg.AI.Stream = boolSetting(values, SettingAIStream, cfg.AI.Stream)
	cfg.AI.ExtraHeaders = stringSetting(values, SettingAIExtraHeaders, cfg.AI.ExtraHeaders)
	cfg.AI.EmbedBaseURL = stringSetting(values, SettingAIEmbedBaseURL, cfg.AI.EmbedBaseURL)
	if raw, ok := values[SettingAITaskModels]; ok {
		if strings.TrimSpace(raw) == "" {
			cfg.AI.TaskModels = nil
		} else {
			var routes map[string]config.AITaskRoute
			if err := json.Unmarshal([]byte(raw), &routes); err == nil {
				cfg.AI.TaskModels = routes
			}
		}
	}
	cfg.Send.MaxPerMinute = intSetting(values, SettingSendMaxMinute, cfg.Send.MaxPerMinute)
	cfg.Send.MaxPerHour = intSetting(values, SettingSendMaxHour, cfg.Send.MaxPerHour)
	cfg.Send.WarnRecipients = intSetting(values, SettingSendWarnRecipients, cfg.Send.WarnRecipients)
	cfg.Send.MaxRetries = intSetting(values, SettingSendMaxRetries, cfg.Send.MaxRetries)
	cfg.Send.RetryBaseSeconds = intSetting(values, SettingSendRetryBase, cfg.Send.RetryBaseSeconds)
	cfg.Send.RetryMaxSeconds = intSetting(values, SettingSendRetryMax, cfg.Send.RetryMaxSeconds)
	cfg.Send.DLPPolicy = stringSetting(values, SettingSendDLPPolicy, cfg.Send.DLPPolicy)
	cfg.Send.DLPKeywords = csvSetting(values, SettingSendDLPKeywords, cfg.Send.DLPKeywords)
	cfg.Compose.WritingGuide = stringSetting(values, SettingComposeWritingGuide, cfg.Compose.WritingGuide)
	cfg.Compose.BannedPhrases = csvSettingRaw(values, SettingComposeBannedPhrases, cfg.Compose.BannedPhrases)
	cfg.Attachments.BlockExtensions = csvSetting(values, SettingAttachmentBlock, cfg.Attachments.BlockExtensions)
	cfg.Attachments.QuarantineExtensions = csvSetting(values, SettingAttachmentQuarantine, cfg.Attachments.QuarantineExtensions)
	cfg.Attachments.ArchiveMaxEntries = intSetting(values, SettingArchiveMaxEntries, cfg.Attachments.ArchiveMaxEntries)
	cfg.Attachments.ArchiveMaxTotalBytes = int64Setting(values, SettingArchiveMaxTotalBytes, cfg.Attachments.ArchiveMaxTotalBytes)
	cfg.Attachments.ArchiveMaxRatio = floatSetting(values, SettingArchiveMaxRatio, cfg.Attachments.ArchiveMaxRatio)
	cfg.AllowInsecureMail = boolSetting(values, SettingAllowInsecureMail, cfg.AllowInsecureMail)
	cfg.AllowPrivateHosts = boolSetting(values, SettingAllowPrivateHosts, cfg.AllowPrivateHosts)
}

func (a *App) AdminSaveSettings(ctx context.Context, values map[string]string, oidcClientSecret string) error {
	a.settingsWriteMu.Lock()
	defer a.settingsWriteMu.Unlock()
	return a.adminSaveSettings(ctx, values, oidcClientSecret)
}

func (a *App) adminSaveSettings(ctx context.Context, values map[string]string, oidcClientSecret string) error {
	if _, err := requireAdmin(ctx); err != nil {
		return err
	}
	stored, err := a.Store.GetSettings(ctx)
	if err != nil {
		return err
	}
	return a.adminSaveSettingsSnapshot(ctx, values, oidcClientSecret, stored)
}

// AdminPatch passes its already revision-checked snapshot here. Rereading it
// before commit would accept another replica's write and silently defeat CAS.
func (a *App) adminSaveSettingsSnapshot(ctx context.Context, values map[string]string, oidcClientSecret string, storedBefore map[string]string) error {
	if _, err := requireAdmin(ctx); err != nil {
		return err
	}
	var newRefs []domain.SecretRef
	committed := false
	defer func() {
		if !committed {
			a.revokeUnreferencedSettingSecrets(newRefs)
		}
	}()
	clean := map[string]string{}
	for key, value := range values {
		if d, ok := settingDefinition(key); ok && d.Apply == "fixed" {
			return userErrf("설정 %s은 항상 적용되는 고정 정책이며 변경할 수 없습니다", key)
		}
		if allowedSettings[key] {
			clean[key] = strings.TrimSpace(value)
		}
	}
	if err := validateSettingValues(clean); err != nil {
		return err
	}
	oldOIDCRef := storedBefore[SettingOIDCSecretRef]
	if oldOIDCRef == "" {
		oldOIDCRef = a.Cfg.Auth.OIDCSecretRef
	}
	_, oidcRefProvided := values[SettingOIDCSecretRef]
	removeOIDCSecret := oidcClientSecret == "" && clean[SettingOIDCSecretRef] == "" && oidcRefProvided
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
	if err := validateTrackingSettings(storedBefore, clean); err != nil {
		return err
	}
	if oidcClientSecret != "" {
		handle := domain.NewSecretHandle([]byte(oidcClientSecret))
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "OIDC client secret", handle)
		handle.Zero()
		if err != nil {
			return err
		}
		clean[SettingOIDCSecretRef] = string(ref)
		newRefs = append(newRefs, ref)
	}
	// The Milvus token arrives as a write-only plaintext field. Register it in
	// the SecretStore and persist only the reference; never store the token in
	// system_settings (P1 Milvus 토큰 보안).
	oldMilvusRef := storedBefore[SettingVectorMilvusTokenRef]
	if token := strings.TrimSpace(values[SettingVectorMilvusToken]); token != "" {
		handle := domain.NewSecretHandle([]byte(token))
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "Milvus access token", handle)
		handle.Zero()
		if err != nil {
			return err
		}
		clean[SettingVectorMilvusTokenRef] = string(ref)
		newRefs = append(newRefs, ref)
	}
	delete(clean, SettingVectorMilvusToken) // never persist the plaintext token
	// The relay password reaches this path as a reference from AdminPatchSettings;
	// a legacy caller sending the plaintext gets it registered the same way, so
	// the settings table never holds it (MAIL-STANDARD: 비밀번호는 되읽히지 않는다).
	if password, ok := clean[notifymail.KeyPassword]; ok && password != "" && !strings.HasPrefix(password, "sec_") {
		handle := domain.NewSecretHandle([]byte(password))
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "알림 SMTP 비밀번호", handle)
		handle.Zero()
		if err != nil {
			return err
		}
		clean[notifymail.KeyPassword] = string(ref)
		newRefs = append(newRefs, ref)
	}
	if storedBefore[SettingVectorMilvusToken] != "" && clean[SettingVectorMilvusTokenRef] != "" {
		clean[SettingVectorMilvusToken] = "" // scrub a pre-migration plaintext row
	}
	hasNewHeaders := clean[SettingAIExtraHeaders] != ""
	if err := a.encryptHeaderSetting(ctx, clean); err != nil {
		return err
	}
	if hasNewHeaders {
		newRefs = append(newRefs, domain.SecretRef(clean["ai.extra_headers_ref"]))
	}
	if err := a.commitAdminSettings(ctx, storedBefore, clean); err != nil {
		return err
	}
	committed = true
	if removeOIDCSecret && oldOIDCRef != "" {
		a.revokeUnreferencedSettingSecrets([]domain.SecretRef{domain.SecretRef(oldOIDCRef)})
	}
	if newRef := clean[SettingVectorMilvusTokenRef]; newRef != "" && oldMilvusRef != "" && oldMilvusRef != newRef {
		a.revokeUnreferencedSettingSecrets([]domain.SecretRef{domain.SecretRef(oldMilvusRef)})
	}
	if stored, err := a.Store.GetSettings(ctx); err == nil {
		a.applyAISettings(stored)
		a.applyRuntimeSettings(stored)
	} else {
		a.applyAISettings(clean)
	}

	hasVectorSetting := false
	for k := range clean {
		if strings.HasPrefix(k, "vector.") {
			hasVectorSetting = true
			break
		}
	}
	if hasVectorSetting {
		a.initVectorStore(ctx)
	}
	if _, ok := clean[SettingMCPPolicy]; ok {
		a.loadMCPPolicy(ctx)
	}

	if notifier, ok := a.Store.(interface{ NotifySettingsChange(ctx context.Context) }); ok {
		notifier.NotifySettingsChange(ctx)
	}

	a.auditSettingsChanges(ctx, storedBefore, clean)
	return nil
}

// validateTrackingSettings checks the tracking configuration that would result
// from a save, so a snippet over the size limit or a provider missing its id
// is refused before it reaches the store.
func validateTrackingSettings(stored, incoming map[string]string) error {
	touched := false
	for _, key := range tracking.SettingKeys {
		if _, ok := incoming[key]; ok {
			touched = true
			break
		}
	}
	if !touched {
		return nil
	}
	merged := make(map[string]string, len(stored)+len(incoming))
	for key, value := range stored {
		merged[key] = value
	}
	for key, value := range incoming {
		merged[key] = value
	}
	if err := tracking.ReadConfig(merged).Validate(); err != nil {
		return userErrf("방문 추적: %v", err)
	}
	return nil
}

// TrackingConfig reads the visitor tracking settings. Any failure reads as
// "off" so a settings outage never breaks a page.
func (a *App) TrackingConfig(ctx context.Context) tracking.Config {
	values, err := a.Store.GetSettings(ctx)
	if err != nil {
		return tracking.Config{}
	}
	return tracking.ReadConfig(values)
}

// AdminAllowTrackingHost appends one blocked origin to the tracking allow
// list — the one-click fix for a policy report on the admin screen.
func (a *App) AdminAllowTrackingHost(ctx context.Context, origin string) error {
	a.settingsWriteMu.Lock()
	defer a.settingsWriteMu.Unlock()
	if _, err := requireAdmin(ctx); err != nil {
		return err
	}
	origin = strings.TrimSpace(origin)
	lower := strings.ToLower(origin)
	if origin == "" || !(strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) || strings.ContainsAny(origin, " ,;'\"") {
		return userErrf("허용할 출처가 올바르지 않습니다: http(s)://호스트 형식이어야 합니다")
	}
	stored, err := a.Store.GetSettings(ctx)
	if err != nil {
		return err
	}
	updated := tracking.AddAllowedHost(stored[tracking.SettingAllowedHosts], origin)
	if err := a.commitAdminSettings(ctx, stored, map[string]string{tracking.SettingAllowedHosts: updated}); err != nil {
		return err
	}
	if notifier, ok := a.Store.(interface{ NotifySettingsChange(ctx context.Context) }); ok {
		notifier.NotifySettingsChange(ctx)
	}
	a.audit(ctx, "settings_update", "system", "ok", "tracking.allowed_hosts+="+origin)
	return nil
}

func (a *App) currentAIConfig() config.AIConfig {
	a.aiConfigMu.RLock()
	defer a.aiConfigMu.RUnlock()
	return a.Cfg.AI
}

type aiConfigurable interface {
	Configure(config.AIConfig)
}

func (a *App) AdminSaveAISettings(ctx context.Context, values map[string]string, apiKey string) error {
	a.settingsWriteMu.Lock()
	defer a.settingsWriteMu.Unlock()
	if _, err := requireAdmin(ctx); err != nil {
		return err
	}
	var newRefs []domain.SecretRef
	committed := false
	defer func() {
		if !committed {
			a.revokeUnreferencedSettingSecrets(newRefs)
		}
	}()
	clean := map[string]string{}
	for key, value := range values {
		if allowedSettings[key] && strings.HasPrefix(key, "ai.") {
			clean[key] = strings.TrimSpace(value)
		}
	}
	if err := validateSettingValues(clean); err != nil {
		return err
	}
	storedBefore, err := a.Store.GetSettings(ctx)
	if err != nil {
		return err
	}
	u, err := url.Parse(clean[SettingAIBaseURL])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return userErrf("AI Base URL must be an absolute HTTP(S) URL")
	}
	if clean[SettingAIModel] == "" {
		return userErrf("AI model is required")
	}
	oldKeyRef, keyWasStored := storedBefore[SettingAIAPIKeyRef]
	if !keyWasStored {
		oldKeyRef = a.initialConfig.AI.APIKeyRef
	}
	requestedKeyRef, keyRefProvided := values[SettingAIAPIKeyRef]
	removeKey := apiKey == "" && keyRefProvided && requestedKeyRef == ""
	if apiKey != "" {
		handle := domain.NewSecretHandle([]byte(apiKey))
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "AI provider API key", handle)
		handle.Zero()
		if err != nil {
			return err
		}
		clean[SettingAIAPIKeyRef] = string(ref)
		newRefs = append(newRefs, ref)
	}
	hasNewHeaders := clean[SettingAIExtraHeaders] != ""
	if err := a.encryptHeaderSetting(ctx, clean); err != nil {
		return err
	}
	if hasNewHeaders {
		newRefs = append(newRefs, domain.SecretRef(clean["ai.extra_headers_ref"]))
	}
	if err := a.commitAdminSettings(ctx, storedBefore, clean); err != nil {
		return err
	}
	committed = true
	if removeKey && oldKeyRef != "" {
		a.revokeUnreferencedSettingSecrets([]domain.SecretRef{domain.SecretRef(oldKeyRef)})
	} else if apiKey != "" && oldKeyRef != "" && oldKeyRef != clean[SettingAIAPIKeyRef] {
		a.revokeUnreferencedSettingSecrets([]domain.SecretRef{domain.SecretRef(oldKeyRef)})
	}
	if stored, err := a.Store.GetSettings(ctx); err == nil {
		a.applyAISettings(stored)
		a.applyRuntimeSettings(stored)
	} else {
		a.applyAISettings(clean)
	}

	if notifier, ok := a.Store.(interface{ NotifySettingsChange(ctx context.Context) }); ok {
		notifier.NotifySettingsChange(ctx)
	}

	cfg := a.currentAIConfig()
	a.audit(ctx, "ai_settings_update", "system:ai", "ok", "model="+cfg.Model)
	a.auditSettingsChanges(ctx, storedBefore, clean)
	return nil
}

func (a *App) encryptHeaderSetting(ctx context.Context, values map[string]string) error {
	value := values[SettingAIExtraHeaders]
	if value == "" {
		return nil
	}
	handle := domain.NewSecretHandle([]byte(value))
	defer handle.Zero()
	ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "AI extra headers", handle)
	if err != nil {
		return err
	}
	values["ai.extra_headers_ref"] = string(ref)
	values[SettingAIExtraHeaders] = ""
	return nil
}

func (a *App) applyAISettings(values map[string]string) {
	hasAI := false
	for key := range values {
		if strings.HasPrefix(key, "ai.") {
			hasAI = true
			break
		}
	}
	if !hasAI {
		return
	}
	wrapper := config.Config{AI: a.currentAIConfig()}
	applyStoredSettings(&wrapper, values)
	a.aiConfigMu.Lock()
	a.Cfg.AI = wrapper.AI
	a.aiConfigMu.Unlock()
	if configurable, ok := a.aiRaw.(aiConfigurable); ok {
		configurable.Configure(wrapper.AI)
	}
}

type AIConnectionResult struct {
	OK        bool   `json:"ok"`
	Model     string `json:"model"`
	LatencyMS int64  `json:"latency_ms"`
	Message   string `json:"message"`
}

func (a *App) AdminTestAI(ctx context.Context) (AIConnectionResult, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return AIConnectionResult{}, err
	}
	start := time.Now()
	result, err := a.AI.Generate(ctx, domain.GenerationRequest{
		System: "You are a connectivity probe. Never include secrets.",
		User:   "Reply with exactly: POSTRA_AI_OK", MaxTokens: 16,
	})
	out := AIConnectionResult{Model: a.currentAIConfig().Model, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		out.Message = providerDiagnostic(err)
		return out, nil
	}
	out.OK = strings.TrimSpace(result.Text) != ""
	if out.OK {
		out.Message = "Chat completion connection is healthy."
	} else {
		out.Message = "Provider responded successfully, but returned an empty completion."
	}
	return out, nil
}

type EmbeddingStoreTestResult struct {
	OK                   bool   `json:"ok"`
	AIEmbedOK            bool   `json:"ai_embed_ok"`
	AIEmbedLatencyMS     int64  `json:"ai_embed_latency_ms"`
	AIEmbedModel         string `json:"ai_embed_model"`
	VectorStoreOK        bool   `json:"vector_store_ok"`
	VectorStoreLatencyMS int64  `json:"vector_store_latency_ms"`
	VectorStoreProvider  string `json:"vector_store_provider"`
	Message              string `json:"message"`
}

func (a *App) AdminTestEmbeddingStore(ctx context.Context) (EmbeddingStoreTestResult, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return EmbeddingStoreTestResult{}, err
	}

	result := EmbeddingStoreTestResult{
		AIEmbedModel: a.currentAIConfig().EmbedModel,
	}

	settings, err := a.Store.GetSettings(ctx)
	if err == nil {
		result.VectorStoreProvider = settings[SettingVectorProvider]
	}
	if result.VectorStoreProvider == "" {
		if _, ok := a.Store.(interface{ HasPgVector() bool }); ok {
			result.VectorStoreProvider = "postgres"
		} else {
			result.VectorStoreProvider = "sqlite"
		}
	}

	embedStart := time.Now()
	embedRes, err := a.AI.Embed(ctx, domain.EmbeddingRequest{
		Input: []string{"postra connectivity probe text for embedding"},
	})
	result.AIEmbedLatencyMS = time.Since(embedStart).Milliseconds()
	if err != nil {
		result.Message = "AI embedding: " + providerDiagnostic(err)
		return result, nil
	}
	if len(embedRes.Vectors) == 0 || len(embedRes.Vectors[0]) == 0 {
		result.Message = "AI embedding succeeded but returned empty vector"
		return result, nil
	}
	result.AIEmbedOK = true

	vectorStart := time.Now()
	err = a.VectorStore().Ping(ctx)
	result.VectorStoreLatencyMS = time.Since(vectorStart).Milliseconds()
	if err != nil {
		result.Message = "Vector store: " + providerDiagnostic(err)
		return result, nil
	}
	result.VectorStoreOK = true
	result.OK = true
	result.Message = fmt.Sprintf("Pipeline is healthy. Embedding Model: %s, Vector Store: %s", result.AIEmbedModel, result.VectorStoreProvider)

	return result, nil
}

func taskModelsJSON(routes map[string]config.AITaskRoute) string {
	if len(routes) == 0 {
		return ""
	}
	b, err := json.Marshal(routes)
	if err != nil {
		return ""
	}
	return string(b)
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

// csvSettingRaw splits a comma-separated setting preserving case and internal
// spaces (used for banned phrases / keywords that are human phrases).
func csvSettingRaw(values map[string]string, key string, fallback []string) []string {
	value, ok := values[key]
	if !ok {
		return fallback
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
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
