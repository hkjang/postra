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
)

const (
	SettingAuthSessionHours       = "auth.session_hours"
	SettingOIDCIssuer             = "auth.oidc.issuer"
	SettingOIDCClientID           = "auth.oidc.client_id"
	SettingOIDCSecretRef          = "auth.oidc.secret_ref"   // #nosec G101 -- setting key, never a credential value
	SettingOIDCRedirectURL        = "auth.oidc.redirect_url" // #nosec G101 -- setting key, not a credential
	SettingOIDCAutoProvision      = "auth.oidc.auto_provision"
	SettingOIDCAdminGroup         = "auth.oidc.admin_group"
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
	SettingOIDCAdminGroup: true, SettingSyncAutoMinutes: true, SettingSyncInitialWindowDays: true,
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
	storedBefore, _ := a.Store.GetSettings(ctx)
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
	if oidcClientSecret != "" {
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "OIDC client secret",
			domain.NewSecretHandle([]byte(oidcClientSecret)))
		if err != nil {
			return err
		}
		clean[SettingOIDCSecretRef] = string(ref)
	}
	// The Milvus token arrives as a write-only plaintext field. Register it in
	// the SecretStore and persist only the reference; never store the token in
	// system_settings (P1 Milvus 토큰 보안).
	if token := strings.TrimSpace(values[SettingVectorMilvusToken]); token != "" {
		oldRef := storedBefore[SettingVectorMilvusTokenRef]
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "Milvus access token",
			domain.NewSecretHandle([]byte(token)))
		if err != nil {
			return err
		}
		clean[SettingVectorMilvusTokenRef] = string(ref)
		if oldRef != "" && oldRef != string(ref) {
			_ = a.RevokeSecret(ctx, domain.SecretRef(oldRef))
		}
	}
	delete(clean, SettingVectorMilvusToken) // never persist the plaintext token
	if err := a.Store.UpsertSettings(ctx, clean); err != nil {
		return err
	}
	if removeOIDCSecret && oldOIDCRef != "" {
		_ = a.RevokeSecret(ctx, domain.SecretRef(oldOIDCRef))
	}
	a.applyAISettings(clean)

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

	a.audit(ctx, "settings_update", "system", "ok", "keys="+strconv.Itoa(len(clean)))
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
	if _, err := requireAdmin(ctx); err != nil {
		return err
	}
	clean := map[string]string{}
	for key, value := range values {
		if allowedSettings[key] && strings.HasPrefix(key, "ai.") {
			clean[key] = strings.TrimSpace(value)
		}
	}
	u, err := url.Parse(clean[SettingAIBaseURL])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return userErrf("AI Base URL must be an absolute HTTP(S) URL")
	}
	if clean[SettingAIModel] == "" {
		return userErrf("AI model is required")
	}
	oldKeyRef := a.currentAIConfig().APIKeyRef
	requestedKeyRef, keyRefProvided := values[SettingAIAPIKeyRef]
	removeKey := apiKey == "" && keyRefProvided && requestedKeyRef == ""
	if apiKey != "" {
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, "AI provider API key",
			domain.NewSecretHandle([]byte(apiKey)))
		if err != nil {
			return err
		}
		clean[SettingAIAPIKeyRef] = string(ref)
	}
	if err := a.Store.UpsertSettings(ctx, clean); err != nil {
		return err
	}
	if removeKey && oldKeyRef != "" {
		_ = a.RevokeSecret(ctx, domain.SecretRef(oldKeyRef))
	}
	a.applyAISettings(clean)

	if notifier, ok := a.Store.(interface{ NotifySettingsChange(ctx context.Context) }); ok {
		notifier.NotifySettingsChange(ctx)
	}

	cfg := a.currentAIConfig()
	a.audit(ctx, "ai_settings_update", "system:ai", "ok", "model="+cfg.Model)
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
		out.Message = err.Error()
		return out, nil
	}
	out.OK = strings.Contains(strings.ToUpper(result.Text), "POSTRA_AI_OK")
	if out.OK {
		out.Message = "Chat completion connection is healthy."
	} else {
		out.Message = fmt.Sprintf("Provider responded, but probe output was unexpected: %.120s", result.Text)
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
		result.Message = fmt.Sprintf("AI embedding failed: %v", err)
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
		result.Message = fmt.Sprintf("AI embedding succeeded but Vector store check failed: %v", err)
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
