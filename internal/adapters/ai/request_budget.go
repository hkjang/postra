package ai

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"postra/internal/domain"
)

var (
	providerContextPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)maximum context length (?:is|of)\s*([0-9]+)`),
		regexp.MustCompile(`(?i)(?:context window|context length|context limit)\s*(?:is|of|:|=)\s*([0-9]+)`),
	}
	providerInputPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)([0-9]+)\s+(?:tokens\s+)?in (?:the )?(?:messages|prompt)`),
		regexp.MustCompile(`(?i)(?:request has|input (?:tokens|length)(?: is|:|=)?)\s*([0-9]+)\s*(?:input tokens|tokens)?`),
		regexp.MustCompile(`(?i)([0-9]+)\s+input tokens`),
	}
	providerOutputPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:max_tokens|max_completion_tokens)[^\n]{0,100}?(?:at most|less than or equal to|<=|maximum (?:is|of)|maximum allowed (?:is|of))\s*([0-9]+)`),
		regexp.MustCompile(`(?i)(?:maximum|max) (?:output|completion) tokens?\s*(?:is|of|:|=)\s*([0-9]+)`),
	}
)

func providerLimit(patterns []*regexp.Regexp, message string) int {
	for _, pattern := range patterns {
		match := pattern.FindStringSubmatch(message)
		if len(match) != 2 {
			continue
		}
		value, err := strconv.ParseInt(match[1], 10, 32)
		if err == nil && value > 0 && value <= 100_000_000 {
			return int(value)
		}
	}
	return 0
}

// Only a structured, pre-generation 400/422 rejection may lead to one retry.
// No transport/5xx or partial-stream retries: generation can incur costs and
// the same input must never be unknowingly submitted an unbounded number of times.
func adjustRejectedRequest(request *chatRequest, raw []byte, limits domain.AIModelLimits, estimatedInput int) (retry, contextRejected bool) {
	var response struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return false, false
	}
	message := response.Error.Message
	lower := strings.ToLower(message)
	if request.MaxTokens > 0 && strings.Contains(lower, "max_tokens") && strings.Contains(lower, "max_completion_tokens") &&
		(strings.Contains(lower, "not supported") || strings.Contains(lower, "unsupported")) &&
		(strings.Contains(lower, "use 'max_completion_tokens'") || strings.Contains(lower, `use "max_completion_tokens"`) || strings.Contains(lower, "use max_completion_tokens")) {
		request.MaxCompletionTokens, request.MaxTokens = request.MaxTokens, 0
		return true, false
	}
	contextCap := providerLimit(providerContextPatterns, message)
	contextRejected = response.Error.Code == "context_length_exceeded" || contextCap > 0
	current := minPositive(request.MaxTokens, request.MaxCompletionTokens)
	if current == 0 {
		return false, contextRejected
	}
	adjusted := current
	if contextCap > 0 {
		// Keep an administrator's smaller manual cap, and honor an already
		// discovered smaller cap. Exact provider prompt counts beat heuristics.
		contextCap = minPositive(contextCap, limits.ContextLength)
		input := providerLimit(providerInputPatterns, message)
		if input == 0 {
			input = estimatedInput
		}
		remaining := contextCap - input
		if remaining <= 0 {
			return false, true
		}
		adjusted = minPositive(adjusted, remaining)
	}
	if outputCap := providerLimit(providerOutputPatterns, message); outputCap > 0 {
		adjusted = minPositive(adjusted, outputCap)
	}
	if adjusted <= 0 || adjusted >= current {
		return false, contextRejected
	}
	if request.MaxCompletionTokens > 0 {
		request.MaxCompletionTokens = adjusted
	} else {
		request.MaxTokens = adjusted
	}
	return true, contextRejected
}

func emptyResponseError() error {
	return &domain.PublicError{Code: "empty_response", Message: "AI 모델이 비어 있는 응답을 반환했습니다. 모델 및 출력 토큰 설정을 확인하세요.", Status: 502}
}

func invalidEmbeddingError() error {
	return &domain.PublicError{Code: "invalid_embedding", Message: "임베딩 모델의 응답 형식이 올바르지 않습니다. 임베딩 모델과 API 호환성을 확인하세요.", Status: 502}
}

func invalidResponseError() error {
	return &domain.PublicError{Code: "invalid_response", Message: "AI 응답이 불완전하거나 올바른 형식이 아닙니다. 결과는 사용하지 않았습니다. AI 서버 상태를 확인한 후 다시 시도하세요.", Status: 502}
}
