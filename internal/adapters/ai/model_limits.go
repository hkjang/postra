package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

const (
	modelLimitsTTL         = 10 * time.Minute
	modelLimitsNegativeTTL = 30 * time.Second
	modelLimitsTimeout     = 3 * time.Second
	modelLimitsCacheSize   = 64
)

type discoveredModelLimits struct {
	context int
	output  int
	status  string
	reason  string
}

type cachedModelLimits struct {
	value   discoveredModelLimits
	expires time.Time
}

type pendingModelLimits struct {
	done  chan struct{}
	value discoveredModelLimits
}

// ModelLimits returns the effective limits, not an unverified model-name guess.
// MaxOutputTokens is the minimum positive configured/task/provider output cap.
// The standard OpenAI models API does not guarantee any context metadata; in
// that case the administrator's ContextLength remains the explicit fallback.
func (p *OpenAICompat) ModelLimits(ctx context.Context, task string, embedding bool) (domain.AIModelLimits, error) {
	cfg, client := p.snapshot()
	route := cfg.RouteForTask(task)
	if embedding {
		route = embeddingRoute(cfg)
	}
	if err := checkAIEndpoint(ctx, route.BaseURL, cfg.AllowExternal); err != nil {
		return domain.AIModelLimits{}, err
	}
	if modelDisabled(cfg, route.Model) {
		return domain.AIModelLimits{}, &domain.PublicError{Code: "model_disabled", Message: "관리자가 비활성화한 AI 모델입니다.", Status: 403}
	}
	meta := discoveredModelLimits{status: "manual"}
	if cfg.AutoContextLength {
		key, extra, err := p.credentials(ctx, cfg, route.APIKeyRef)
		if err != nil {
			return domain.AIModelLimits{}, err
		}
		meta, err = p.discoverModelLimits(ctx, cfg, client, route, key, extra)
		if err != nil {
			return domain.AIModelLimits{}, err
		}
	}
	return effectiveModelLimits(cfg, route, meta, embedding), nil
}

func embeddingRoute(cfg config.AIConfig) config.AITaskRoute {
	route := cfg.RouteForTask("")
	if cfg.EmbedModel != "" {
		route.Model = cfg.EmbedModel
	}
	if cfg.EmbedBaseURL != "" {
		route.BaseURL = cfg.EmbedBaseURL
	}
	return route
}

func effectiveModelLimits(cfg config.AIConfig, route config.AITaskRoute, meta discoveredModelLimits, embedding bool) domain.AIModelLimits {
	out := domain.AIModelLimits{Model: route.Model, ContextLength: cfg.ContextLength, Source: "config", Status: meta.status, Reason: meta.reason}
	if embedding {
		out.ContextLength = 0
	} // A chat setting is not an embedding-model limit.
	if meta.context > 0 {
		out.ContextLength, out.Source = meta.context, "models"
	}
	if !embedding {
		out.MaxOutputTokens = minPositive(cfg.MaxTokens, route.MaxTokens, meta.output)
		if out.MaxOutputTokens == 0 {
			out.MaxOutputTokens = 4096
		}
	}
	return out
}

func minPositive(values ...int) int {
	result := 0
	for _, value := range values {
		if value > 0 && (result == 0 || value < result) {
			result = value
		}
	}
	return result
}

func (p *OpenAICompat) credentials(ctx context.Context, cfg config.AIConfig, ref string) (key, extra string, err error) {
	if ref != "" {
		if p.secrets == nil {
			return "", "", credentialError(ctx, nil)
		}
		h, acquireErr := p.secrets.Acquire(ctx, domain.SecretRef(ref), domain.PurposeAIKey)
		if acquireErr != nil {
			return "", "", credentialError(ctx, acquireErr)
		}
		key = string(h.Reveal())
		h.Zero()
	}
	extra, err = p.extraHeaders(ctx, cfg)
	return
}

func modelLimitsKey(cfg config.AIConfig, route config.AITaskRoute, key, extra string) [32]byte {
	// Hash the actual credential too: rotating a secret under the same reference
	// must not reuse metadata obtained with the previous gateway identity.
	identity, _ := json.Marshal([]any{strings.TrimRight(route.BaseURL, "/"), route.Model, key, extra, cfg.AllowExternal})
	return sha256.Sum256(identity)
}

func (p *OpenAICompat) discoverModelLimits(ctx context.Context, cfg config.AIConfig, client *http.Client, route config.AITaskRoute, key, extra string) (discoveredModelLimits, error) {
	if err := ctx.Err(); err != nil {
		return discoveredModelLimits{}, err
	}
	cacheKey := modelLimitsKey(cfg, route, key, extra)
	p.limitsMu.Lock()
	if cached, ok := p.limitsCache[cacheKey]; ok && time.Now().Before(cached.expires) {
		p.limitsMu.Unlock()
		return cached.value, nil
	}
	if p.limitsPending == nil {
		p.limitsPending = make(map[[32]byte]*pendingModelLimits)
	}
	pending, exists := p.limitsPending[cacheKey]
	if !exists {
		pending = &pendingModelLimits{done: make(chan struct{})}
		p.limitsPending[cacheKey] = pending
		// A cancelled initiating request must not cancel other callers' discovery.
		// This metadata-only request is detached but strictly time bounded.
		go func() {
			lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelLimitsTimeout)
			defer cancel()
			value := fetchModelLimits(lookupCtx, cfg, client, route, key, extra)
			p.limitsMu.Lock()
			p.cacheModelLimitsLocked(cacheKey, value)
			pending.value = value
			delete(p.limitsPending, cacheKey)
			close(pending.done)
			p.limitsMu.Unlock()
		}()
	}
	p.limitsMu.Unlock()
	select {
	case <-ctx.Done():
		return discoveredModelLimits{}, ctx.Err()
	case <-pending.done:
		return pending.value, nil
	}
}

func (p *OpenAICompat) cacheModelLimitsLocked(key [32]byte, value discoveredModelLimits) {
	if p.limitsCache == nil {
		p.limitsCache = make(map[[32]byte]cachedModelLimits)
	}
	if len(p.limitsCache) >= modelLimitsCacheSize {
		var oldestKey [32]byte
		var oldest time.Time
		for candidate, cached := range p.limitsCache {
			if oldest.IsZero() || cached.expires.Before(oldest) {
				oldestKey, oldest = candidate, cached.expires
			}
		}
		delete(p.limitsCache, oldestKey)
	}
	ttl := modelLimitsTTL
	if value.status != "detected" {
		ttl = modelLimitsNegativeTTL
	}
	p.limitsCache[key] = cachedModelLimits{value: value, expires: time.Now().Add(ttl)}
}

func fetchModelLimits(ctx context.Context, cfg config.AIConfig, client *http.Client, route config.AITaskRoute, key, extra string) discoveredModelLimits {
	fallback := discoveredModelLimits{status: "unavailable", reason: "request_rejected"}
	if checkAIEndpoint(ctx, route.BaseURL, cfg.AllowExternal) != nil {
		return fallback
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(route.BaseURL, "/")+"/models", nil)
	if err != nil {
		return fallback
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	injectExtraHeaders(req.Header, extra)
	response, err := client.Do(req)
	if err != nil {
		fallback.reason = providerNetworkReason(err)
		return fallback
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fallback.reason = metadataHTTPReason(response.StatusCode)
		return fallback
	}
	fallback.reason = "invalid_response"
	const maxMetadataBytes = 2 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil || len(body) > maxMetadataBytes {
		return fallback
	}
	var listing struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &listing) != nil {
		return fallback
	}
	if listing.Data == nil {
		return fallback
	}
	for _, model := range listing.Data {
		var id string
		if json.Unmarshal(model["id"], &id) != nil || id != route.Model {
			continue
		}
		result := discoveredModelLimits{status: "unavailable", reason: "no_metadata"}
		readModelLimits(model, &result)
		if result.context > 0 {
			result.status = "detected"
			result.reason = ""
		}
		return result
	}
	return discoveredModelLimits{status: "model_not_found"}
}

func readModelLimits(model map[string]json.RawMessage, result *discoveredModelLimits) {
	// Do not use training lengths (e.g. llama.cpp meta.n_ctx_train), architecture
	// names, model aliases, or another model's metadata as deployment limits.
	for _, field := range []string{"max_model_len", "context_length", "max_context_length", "context_window", "max_context_tokens", "max_input_tokens"} {
		result.context = minPositive(result.context, positiveModelLimit(model[field]))
	}
	for _, field := range []string{"max_output_tokens", "max_completion_tokens"} {
		result.output = minPositive(result.output, positiveModelLimit(model[field]))
	}
	for _, field := range []string{"limits", "model_info"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(model[field], &nested) != nil {
			continue
		}
		// Only one documented wrapper level, not arbitrary recursive JSON.
		for _, key := range []string{"max_model_len", "context_length", "max_context_length", "context_window", "max_context_tokens", "max_input_tokens"} {
			result.context = minPositive(result.context, positiveModelLimit(nested[key]))
		}
		for _, key := range []string{"max_output_tokens", "max_completion_tokens"} {
			result.output = minPositive(result.output, positiveModelLimit(nested[key]))
		}
	}
}

func positiveModelLimit(raw json.RawMessage) int {
	value := strings.Trim(string(raw), "\"")
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n <= 0 || n > 100_000_000 {
		return 0
	}
	return int(n)
}

// estimateInputTokens is deliberately Unicode-aware, but is not a substitute
// for the serving model's tokenizer. Exact provider rejection counts take
// precedence in the one safe pre-generation retry. Mail is never truncated.
func estimateInputTokens(text string) int {
	ascii, nonASCII := 0, 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII += utf8.RuneLen(r)
		}
	}
	return (ascii+2)/3 + nonASCII
}

func estimatePromptTokens(messages []chatMessage) int {
	total := 16
	for _, message := range messages {
		total += estimateInputTokens(message.Content) + 8
	}
	return total
}

func contextLimitError() error {
	return &domain.PublicError{Code: "context_limit", Message: "AI 모델의 입력 한도를 초과했습니다. 메일이나 검색 범위를 줄여 다시 시도하세요. 모델이 한도 정보를 제공하지 않으면 관리자 Context Length 설정을 확인하세요.", Status: 400}
}

func outputLimitError() error {
	return &domain.PublicError{Code: "output_limit", Message: "AI 응답이 모델의 출력 한도에 도달했습니다. 불완전한 결과는 사용하지 않았습니다. 요청 범위를 줄이거나 최대 출력 토큰 설정을 확인하세요.", Status: 400}
}

func budgetOutputTokens(requested int, limits domain.AIModelLimits, promptTokens int) (int, error) {
	maxTokens := minPositive(requested, limits.MaxOutputTokens)
	if maxTokens == 0 {
		maxTokens = 4096
	}
	if limits.ContextLength > 0 {
		available := limits.ContextLength - promptTokens
		if available <= 0 {
			return 0, contextLimitError()
		}
		maxTokens = minPositive(maxTokens, available)
	}
	return maxTokens, nil
}

var _ domain.AIModelLimitsProvider = (*OpenAICompat)(nil)
