package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

type settingsState struct {
	sync.RWMutex
	values    map[string]string
	overrides map[string]string
}

type EffectiveSetting struct {
	SettingDefinition
	Value          string `json:"value"`
	Source         string `json:"source"`
	Locked         bool   `json:"locked"`
	Registered     bool   `json:"registered,omitempty"`
	PendingRestart bool   `json:"pending_restart,omitempty"`
	ActiveValue    string `json:"active_value,omitempty"`
}
type SettingsView struct {
	Fields   []EffectiveSetting `json:"fields"`
	Revision string             `json:"revision"`
	Runtime  map[string]string  `json:"runtime,omitempty"`
}
type SettingsPatch struct {
	Values   map[string]string `json:"values"`
	Secrets  map[string]string `json:"secrets,omitempty"`
	Locks    map[string]bool   `json:"locks,omitempty"`
	Reset    []string          `json:"reset,omitempty"`
	Revision string            `json:"revision,omitempty"`
}

func (a *App) applyRuntimeSettings(stored map[string]string) {
	values := map[string]string{}
	for _, d := range SettingsDefinitions() {
		values[d.Key] = d.Default
	}
	for key, value := range config.BoundValues(a.initialConfig) {
		values[key] = value
	}
	for key, value := range stored {
		if d, ok := settingDefinition(key); ok && (d.Apply == "deployment" || d.Apply == "fixed") {
			continue
		}
		if !strings.HasPrefix(key, "internal.") {
			values[key] = value
		}
	}
	a.runtimeSettings.Lock()
	a.runtimeSettings.values = values
	a.runtimeSettings.overrides = make(map[string]string, len(stored))
	for key, value := range stored {
		if !strings.HasPrefix(key, "internal.") {
			a.runtimeSettings.overrides[key] = value
		}
	}
	a.runtimeSettings.Unlock()
}

// Setting returns administrator-effective values. It never reads environment
// variables during a request. User/account overrides use EffectiveMailPreferences.
func (a *App) Setting(key string) string {
	a.runtimeSettings.RLock()
	v, ok := a.runtimeSettings.values[key]
	a.runtimeSettings.RUnlock()
	if ok {
		return v
	}
	if value, exists := config.BoundValues(a.Cfg)[key]; exists {
		return value
	}
	if d, exists := settingDefinition(key); exists {
		return d.Default
	}
	return ""
}
func (a *App) SettingBool(key string) bool { return a.Setting(key) == "true" }
func (a *App) SettingInt(key string) int   { n, _ := strconv.Atoi(a.Setting(key)); return n }

// EffectiveConfig is a request-time immutable copy. Startup-only settings stay
// at their running values until a new process is constructed.
func (a *App) EffectiveConfig() config.Config {
	a.aiConfigMu.RLock()
	cfg := a.Cfg
	a.aiConfigMu.RUnlock()
	live := map[string]string{}
	a.runtimeSettings.RLock()
	for _, d := range SettingsDefinitions() {
		if d.Apply == "live" && d.Scope == "admin" {
			if value, ok := a.runtimeSettings.overrides[d.Key]; ok {
				live[d.Key] = value
			}
		}
	}
	a.runtimeSettings.RUnlock()
	config.ApplyValues(&cfg, live)
	return cfg
}

func settingsRevision(values map[string]string) string {
	public := domain.AdminSettingsSnapshot(values)
	raw, _ := json.Marshal(public)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (a *App) initialSetting(d SettingDefinition) (string, string) {
	if d.Apply == "fixed" {
		return d.Default, "fixed_policy"
	}
	base := config.BoundValues(a.initialConfig)
	value, ok := base[d.Key]
	if !ok {
		value = d.Default
	}
	source := a.initialConfig.Sources[d.Key]
	if source == "" {
		source = "default"
		if value != d.Default {
			source = "configuration"
		}
	}
	return value, source
}

func (a *App) AdminSettingsCatalog(ctx context.Context) (SettingsView, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return SettingsView{}, err
	}
	stored, err := a.Store.GetSettings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	active := config.BoundValues(a.EffectiveConfig())
	out := SettingsView{Revision: settingsRevision(stored), Fields: []EffectiveSetting{}}
	for _, d := range SettingsDefinitions() {
		value, source := a.initialSetting(d)
		if d.Type == "account" || d.Type == "signature" {
			continue
		} // identifiers belong to one user's namespace
		if v, ok := stored[d.Key]; ok && d.Apply != "deployment" && d.Apply != "fixed" {
			value = v
			source = "admin"
		}
		if d.Key == "sync.triage_mode" {
			if _, explicit := stored[d.Key]; !explicit && a.EffectiveConfig().Sync.AutoTriage {
				value = "all"
				source = "configuration"
			}
		}
		f := EffectiveSetting{SettingDefinition: d, Value: value, Source: source, Locked: d.Apply == "deployment" || d.Apply == "fixed" || stored["policy.lock."+d.Key] == "true"}
		if d.Apply == "restart" {
			running := active[d.Key]
			if initial, ok := a.startedSettings[d.Key]; ok {
				running = initial
			}
			f.ActiveValue = running
			f.PendingRestart = running != value
		}
		if d.Secret {
			f.Registered = value != ""
			if d.Key == "ai.extra_headers" {
				f.Registered = f.Registered || stored["ai.extra_headers_ref"] != ""
			}
			f.Value = ""
			f.ActiveValue = ""
			f.Default = ""
		}
		out.Fields = append(out.Fields, f)
	}
	return out, nil
}

// AdminPatchSettings is shared by REST and any explicitly authorized
// administrative caller. Secrets are new values only and never read back.
func (a *App) AdminPatchSettings(ctx context.Context, patch SettingsPatch) (SettingsView, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return SettingsView{}, err
	}
	a.settingsWriteMu.Lock()
	defer a.settingsWriteMu.Unlock()
	stored, err := a.Store.GetSettings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	if patch.Revision != "" && patch.Revision != settingsRevision(stored) {
		return SettingsView{}, settingsConflict()
	}
	values := map[string]string{}
	var newRefs []domain.SecretRef
	committed := false
	defer func() {
		if !committed {
			a.revokeUnreferencedSettingSecrets(newRefs)
		}
	}()
	for key, value := range patch.Values {
		if d, ok := settingDefinition(key); ok && d.Secret {
			return SettingsView{}, userErrf("비밀값은 쓰기 전용 secrets 항목을 사용하세요")
		}
		values[key] = strings.TrimSpace(value)
	}
	for key, locked := range patch.Locks {
		values["policy.lock."+key] = strconv.FormatBool(locked)
	}
	if len(patch.Reset) > 0 {
		return SettingsView{}, userErrf("관리자 설정은 변경할 실제 값을 지정하세요")
	}
	if err := validateSettingValues(values); err != nil {
		return SettingsView{}, err
	}
	for key, value := range patch.Secrets {
		if value == "" {
			continue
		} // empty always preserves the stored secret
		d, ok := settingDefinition(key)
		if !ok || !d.Secret || d.Apply == "deployment" || d.Apply == "fixed" {
			return SettingsView{}, userErrf("지원하지 않는 비밀값 설정입니다")
		}
		if len(value) > 65536 {
			return SettingsView{}, userErrf("비밀값이 너무 큽니다")
		}
		if key == "ai.extra_headers" || key == "ai.extra_headers_ref" {
			if err := validateExtraHeaders(value); err != nil {
				return SettingsView{}, err
			}
		}
	}
	for key, value := range patch.Secrets {
		if value == "" {
			continue
		}
		storeKey := key
		if key == "ai.extra_headers" || key == "ai.extra_headers_ref" {
			storeKey = "ai.extra_headers_ref"
			if err := validateExtraHeaders(value); err != nil {
				return SettingsView{}, err
			}
		}
		handle := domain.NewSecretHandle([]byte(value))
		ref, err := a.RegisterSecret(ctx, domain.SecretAPIKey, key, handle)
		handle.Zero()
		if err != nil {
			return SettingsView{}, err
		}
		newRefs = append(newRefs, ref)
		values[storeKey] = string(ref)
		if storeKey == "ai.extra_headers_ref" {
			values["ai.extra_headers"] = ""
		}
	}
	if err := a.adminSaveSettingsSnapshot(ctx, values, "", stored); err != nil {
		return SettingsView{}, err
	}
	committed = true
	return a.AdminSettingsCatalog(ctx)
}

func preferencePrefix(userID, accountID string) string {
	prefix := "internal.preferences." + base64.RawURLEncoding.EncodeToString([]byte(userID)) + "."
	if accountID != "" {
		prefix += "account." + base64.RawURLEncoding.EncodeToString([]byte(accountID)) + "."
	} else {
		prefix += "user."
	}
	return prefix
}
func preferenceValue(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	var row struct {
		Value string `json:"value"`
	}
	if json.Unmarshal([]byte(raw), &row) != nil {
		return "", false
	}
	return row.Value, true
}
func accountPreferenceAllowed(d SettingDefinition) bool {
	return d.Scope == "account" || d.Scope == "user" && (strings.HasPrefix(d.Key, "compose.") || strings.HasPrefix(d.Key, "ai."))
}

func (a *App) PersonalSettings(ctx context.Context, accountID string) (SettingsView, error) {
	userID := userIDFrom(ctx)
	if accountID != "" {
		if _, err := a.GetAccount(ctx, accountID); err != nil {
			return SettingsView{}, err
		}
	}
	stored, err := a.Store.GetSettings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	userPrefix, accountPrefix := preferencePrefix(userID, ""), preferencePrefix(userID, accountID)
	out := SettingsView{Fields: []EffectiveSetting{}, Runtime: map[string]string{}}
	for _, key := range []string{"general.product_name", "notifications.enabled", "notifications.poll_seconds", "mail.external_images", "mail.outbound_images", "mail.html_enabled", "sync.min_sync_minutes"} {
		out.Runtime[key] = a.Setting(key)
	}
	for _, d := range SettingsDefinitions() {
		if d.Scope != "user" && d.Scope != "account" {
			continue
		}
		if accountID == "" && d.Scope == "account" {
			continue
		}
		if accountID != "" && !accountPreferenceAllowed(d) {
			continue
		}
		value, source := a.initialSetting(d)
		if d.Key == "ui.language" && a.Setting("general.default_language") != "ko" {
			value = a.Setting("general.default_language")
			source = "admin"
		}
		if v, ok := stored[d.Key]; ok {
			value = v
			source = "admin"
		}
		locked := stored["policy.lock."+d.Key] == "true"
		if !locked {
			if v, ok := preferenceValue(stored[userPrefix+d.Key]); ok {
				value = v
				source = "user"
			}
			if accountID != "" {
				if v, ok := preferenceValue(stored[accountPrefix+d.Key]); ok {
					value = v
					source = "account"
				}
			}
		} else {
			source = "admin_policy"
		}
		if d.Key == "compose.format" && a.Setting("mail.html_enabled") == "false" {
			value = "text"
			source = "admin_policy"
			locked = true
		}
		if strings.HasPrefix(d.Key, "notifications.") && !a.SettingBool("notifications.enabled") {
			value = "false"
			source = "admin_policy"
			locked = true
		}
		out.Fields = append(out.Fields, EffectiveSetting{SettingDefinition: d, Value: value, Source: source, Locked: locked})
	}
	raw, _ := json.Marshal(struct {
		UserID    string             `json:"user_id"`
		AccountID string             `json:"account_id"`
		Fields    []EffectiveSetting `json:"fields"`
	}{userID, accountID, out.Fields})
	sum := sha256.Sum256(raw)
	out.Revision = hex.EncodeToString(sum[:])
	return out, nil
}
func (a *App) EffectiveMailPreferences(ctx context.Context, accountID string) (map[string]string, error) {
	view, err := a.PersonalSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, f := range view.Fields {
		values[f.Key] = f.Value
	}
	return values, nil
}

func (a *App) SavePersonalSettings(ctx context.Context, accountID string, patch SettingsPatch) (SettingsView, error) {
	a.settingsWriteMu.Lock()
	defer a.settingsWriteMu.Unlock()
	view, err := a.PersonalSettings(ctx, accountID)
	if err != nil {
		return SettingsView{}, err
	}
	if patch.Revision != "" && patch.Revision != view.Revision {
		return SettingsView{}, userErrf("설정이 변경되었습니다. 새로고침 후 저장하세요")
	}
	if len(patch.Secrets) > 0 || len(patch.Locks) > 0 {
		return SettingsView{}, userErrf("개인 설정에서 관리자 정책이나 비밀값을 변경할 수 없습니다")
	}
	defs := map[string]EffectiveSetting{}
	for _, f := range view.Fields {
		defs[f.Key] = f
	}
	updates := map[string]string{}
	prefix := preferencePrefix(userIDFrom(ctx), accountID)
	for key, value := range patch.Values {
		field, ok := defs[key]
		if !ok || field.Locked {
			return SettingsView{}, userErrf("변경할 수 없는 개인 설정입니다: %s", key)
		}
		if err := validateSetting(field.SettingDefinition, value); err != nil {
			return SettingsView{}, err
		}
		if key == "mail.default_account_id" && value != "" {
			if _, err := a.GetAccount(ctx, value); err != nil {
				return SettingsView{}, err
			}
		}
		if key == "compose.signature_id" && value != "" {
			signature, err := a.GetMailSignature(ctx, value)
			if err != nil {
				return SettingsView{}, err
			}
			if signature.AccountID != "" && signature.AccountID != accountID {
				return SettingsView{}, userErrf("계정 전용 서명은 해당 계정에서만 기본값으로 지정할 수 있습니다")
			}
		}
		raw, _ := json.Marshal(map[string]string{"value": value})
		updates[prefix+key] = string(raw)
	}
	for _, key := range patch.Reset {
		if field, ok := defs[key]; !ok || field.Locked {
			return SettingsView{}, userErrf("지원하지 않는 개인 설정입니다")
		}
		updates[prefix+key] = ""
	}
	if err := a.Store.UpsertSettings(ctx, updates); err != nil {
		return SettingsView{}, err
	}
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, strings.TrimPrefix(key, prefix))
	}
	sort.Strings(keys)
	raw, _ := json.Marshal(map[string]any{"keys": keys, "account_id": accountID})
	a.audit(ctx, "preferences_update", "preferences", "ok", string(raw))
	return a.PersonalSettings(ctx, accountID)
}

func (a *App) auditSettingsChanges(ctx context.Context, before, after map[string]string) {
	changes := map[string]any{}
	for key, value := range after {
		if before[key] == value {
			continue
		}
		d, _ := settingDefinition(key)
		if d.Secret || strings.Contains(key, "secret") || strings.Contains(key, "token") || strings.Contains(key, "api_key") || strings.Contains(key, "headers") {
			changes[key] = map[string]any{"before_registered": before[key] != "", "after_registered": value != ""}
		} else {
			old, newValue := before[key], value
			if d.Type == "url" {
				for _, target := range []*string{&old, &newValue} {
					if u, err := url.Parse(*target); err == nil {
						u.User, u.RawQuery, u.Fragment = nil, "", ""
						*target = u.String()
					} else {
						*target = "[invalid URL changed]"
					}
				}
			}
			if len(old) > 256 {
				old = "[long value changed]"
			}
			if len(newValue) > 256 {
				newValue = "[long value changed]"
			}
			changes[key] = map[string]string{"before": old, "after": newValue}
		}
	}
	raw, _ := json.Marshal(changes)
	a.audit(ctx, "settings_update", "system", "ok", string(raw))
}
