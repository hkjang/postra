package application

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	aiadapter "postra/internal/adapters/ai"
	"postra/internal/domain"
	"postra/internal/platform/config"
)

type SettingsProbe struct {
	Target  string            `json:"target"`
	Task    string            `json:"task,omitempty"`
	Values  map[string]string `json:"values,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// ProbeSettings validates a candidate without changing settings, credentials,
// scheduled work or active sessions. It never sends real mail content.
func (a *App) ProbeSettings(ctx context.Context, input SettingsProbe) (AIConnectionResult, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return AIConnectionResult{}, err
	}
	if len(input.Task) > 100 || strings.ContainsAny(input.Task, "\x00\r\n") {
		return AIConnectionResult{}, userErrf("AI 작업 이름이 올바르지 않습니다")
	}
	if err := validateSettingValues(input.Values); err != nil {
		return AIConnectionResult{}, err
	}
	for key := range input.Values {
		if def, ok := settingDefinition(key); ok && def.Secret {
			return AIConnectionResult{}, userErrf("비밀값은 쓰기 전용 입력을 사용하세요")
		}
	}
	for key, value := range input.Secrets {
		if len(value) > 65536 {
			return AIConnectionResult{}, userErrf("비밀값이 너무 큽니다")
		}
		if key != "ai.api_key_ref" && key != "ai.extra_headers_ref" && key != "ai.extra_headers" {
			return AIConnectionResult{}, userErrf("연결 테스트에서 지원하지 않는 비밀값입니다")
		}
	}
	cfg := a.EffectiveConfig()
	config.ApplyValues(&cfg, input.Values)
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(min(max(cfg.AI.TimeoutSec, 1), 120))*time.Second)
	defer cancel()
	out := AIConnectionResult{Model: cfg.AI.Model}
	finish := func(err error) (AIConnectionResult, error) {
		out.LatencyMS = time.Since(start).Milliseconds()
		if err != nil {
			out.OK = false
			out.Message = providerDiagnostic(err)
		}
		result := "ok"
		if !out.OK {
			result = "failed"
		}
		a.audit(ctx, "settings_connection_test", "settings:"+input.Target, result, "")
		return out, nil
	}
	switch input.Target {
	case "database":
		out.OK = true
		out.Message = "현재 데이터베이스 연결이 정상입니다."
		return finish(a.Ready(ctx))
	case "oidc":
		issuer := strings.TrimRight(cfg.Auth.OIDCIssuer, "/")
		if err := validateSetting(SettingDefinition{Key: "auth.oidc.issuer", Type: "url"}, issuer); err != nil {
			return out, err
		}
		if issuer == "" {
			return out, userErrf("OIDC Issuer URL을 먼저 지정하세요")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
		if err != nil {
			return finish(err)
		}
		client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			return finish(err)
		}
		defer resp.Body.Close()
		var document struct {
			Issuer        string `json:"issuer"`
			Authorization string `json:"authorization_endpoint"`
			Token         string `json:"token_endpoint"`
			JWKS          string `json:"jwks_uri"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&document) != nil || strings.TrimRight(document.Issuer, "/") != issuer || document.Authorization == "" || document.Token == "" || document.JWKS == "" {
			return finish(userErrf("OIDC discovery invalid"))
		}
		out.OK = true
		out.Message = "OIDC Discovery를 확인했습니다. Client 인증·Callback 등록은 실제 SSO 로그인으로 별도 확인하세요."
		return finish(nil)
	case "ai", "embedding", "ai_models", "embedding_models":
		embedding := input.Target == "embedding" || input.Target == "embedding_models"
		modelsOnly := input.Target == "ai_models" || input.Target == "embedding_models"
		candidateKey := input.Secrets["ai.api_key_ref"]
		if !embedding && input.Task != "" {
			// Test exactly the selected route, without a successful default-model
			// fallback masking a broken task-specific endpoint or credential.
			route := cfg.AI.RouteForTask(input.Task)
			if cfg.AI.TaskModels[input.Task].APIKeyRef != "" {
				// A global candidate key must not replace an explicitly routed
				// credential or be sent to that task's different deployment.
				candidateKey = ""
			}
			cfg.AI.BaseURL, cfg.AI.Model, cfg.AI.APIKeyRef = route.BaseURL, route.Model, route.APIKeyRef
			if route.MaxTokens > 0 && (cfg.AI.MaxTokens <= 0 || route.MaxTokens < cfg.AI.MaxTokens) {
				cfg.AI.MaxTokens = route.MaxTokens
			}
			cfg.AI.TaskModels = nil
		}
		out.Model = cfg.AI.Model
		endpoint := cfg.AI.BaseURL
		if embedding && cfg.AI.EmbedBaseURL != "" {
			endpoint = cfg.AI.EmbedBaseURL
		}
		if !cfg.AI.AllowExternal && !localAIEndpoint(ctx, endpoint) {
			return out, userErrf("외부 AI 호출이 조직 정책에서 허용되지 않았습니다")
		}
		headers := input.Secrets["ai.extra_headers_ref"]
		if headers == "" {
			headers = input.Secrets["ai.extra_headers"]
		}
		if headers != "" {
			if err := validateExtraHeaders(headers); err != nil {
				return out, err
			}
			cfg.AI.ExtraHeaders = headers
			cfg.AI.ExtraHeadersRef = ""
		}
		provider := aiadapter.NewPreview(cfg.AI, a.Secrets, candidateKey)
		if modelsOnly {
			limits, err := queryAIModelLimits(ctx, provider, cfg.AI, "", embedding)
			if err != nil {
				return finish(err)
			}
			out.Model, out.Limits = limits.Model, &limits
			out.OK = limits.Status == "detected" || limits.Status == "manual"
			out.Message = modelLimitsDiagnostic(limits)
			return finish(nil)
		}
		if !embedding {
			result, err := provider.Generate(ctx, domain.GenerationRequest{System: "You are a connectivity probe. Never include secrets.", User: "Reply with exactly POSTRA_AI_OK", MaxTokens: 256})
			if limits, lookupErr := queryAIModelLimits(ctx, provider, cfg.AI, "", false); lookupErr == nil {
				out.Limits = &limits
			}
			out.OK = strings.TrimSpace(result.Text) != ""
			out.Message = "Chat 연결이 정상입니다."
			if !out.OK && err == nil {
				out.Message = "서버 응답은 성공했으나 생성된 텍스트가 없습니다."
			}
			return finish(err)
		}
		result, err := provider.Embed(ctx, domain.EmbeddingRequest{Input: []string{"Postra connection test"}})
		if limits, lookupErr := queryAIModelLimits(ctx, provider, cfg.AI, "", true); lookupErr == nil {
			out.Limits = &limits
		}
		out.OK = len(result.Vectors) == 1 && len(result.Vectors[0]) > 0
		out.Model = result.Model
		out.Message = "Embedding 연결이 정상입니다."
		if !out.OK && err == nil {
			out.Message = "서버 응답은 성공했으나 벡터가 없습니다."
		}
		return finish(err)
	default:
		return out, userErrf("지원하지 않는 연결 테스트입니다")
	}
}
