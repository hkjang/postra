package application

import (
	"context"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

// Keep diagnostics on the same provider path as generation; do not construct
// another live client or persist provider metadata as administrator policy.
func queryAIModelLimits(ctx context.Context, provider domain.AIProvider, cfg config.AIConfig, task string, embedding bool) (domain.AIModelLimits, error) {
	if inspector, ok := provider.(domain.AIModelLimitsProvider); ok {
		return inspector.ModelLimits(ctx, task, embedding)
	}
	route := cfg.RouteForTask(task)
	limits := domain.AIModelLimits{Model: route.Model, ContextLength: cfg.ContextLength, MaxOutputTokens: route.MaxTokens, Source: "config", Status: "unavailable", Reason: "not_supported"}
	if cfg.MaxTokens > 0 && (limits.MaxOutputTokens <= 0 || limits.MaxOutputTokens > cfg.MaxTokens) {
		limits.MaxOutputTokens = cfg.MaxTokens
	}
	if embedding {
		limits.Model, limits.MaxOutputTokens, limits.ContextLength = cfg.EmbedModel, 0, 0
		if limits.Model == "" {
			limits.Model = cfg.Model
		}
	}
	if !cfg.AutoContextLength {
		limits.Status = "manual"
		limits.Reason = ""
	}
	return limits, nil
}

func modelLimitsDiagnostic(limits domain.AIModelLimits) string {
	unknownEmbedding := limits.ContextLength == 0 && limits.MaxOutputTokens == 0
	fallback := "설정된 대체 한도를 사용합니다."
	if unknownEmbedding {
		fallback = "임베딩 입력 한도는 미확인이며 Chat Context 설정을 대신 적용하지 않습니다."
	}
	switch limits.Status {
	case "detected":
		return "선택한 모델의 한도를 확인했습니다. 요청 출력은 설정 상한과 남은 Context에 맞춰 자동 조정됩니다."
	case "manual":
		if unknownEmbedding {
			return "자동 감지가 꺼져 있습니다. " + fallback + " AI 생성 요청은 보내지 않았습니다."
		}
		return "자동 감지가 꺼져 있어 수동 Context 한도를 적용합니다. AI 생성 요청은 보내지 않았습니다."
	case "model_not_found":
		return "모델 목록에서 선택한 ID를 찾지 못했습니다. 모델 이름을 확인하세요. " + fallback
	default:
		// Use only locally owned text. Unknown reasons must never echo a server
		// response or claim that a separate Chat/Embedding request failed.
		reason := map[string]string{
			"not_supported":    "서버가 모델 목록 조회를 지원하지 않거나 경로가 없습니다. Base URL을 확인하세요.",
			"no_metadata":      "모델 목록은 확인했지만 서버가 Context 한도를 제공하지 않습니다.",
			"auth_failed":      "모델 목록 조회의 AI 인증에 실패했습니다. API Key 또는 게이트웨이 인증을 확인하세요.",
			"forbidden":        "모델 목록을 조회할 권한이 없습니다. AI 서버의 접근 정책을 확인하세요.",
			"rate_limited":     "모델 목록 조회에 요청 제한이 적용되었습니다. 요청 빈도와 계정 할당량을 확인하세요.",
			"timeout":          "모델 목록 조회 시간이 초과되었습니다. AI 서버 응답 상태를 확인하세요.",
			"unreachable":      "모델 목록 서버에 연결할 수 없습니다. 주소·내부 DNS·네트워크·인증서를 확인하세요.",
			"invalid_response": "모델 목록 응답 형식이 올바르지 않습니다. API 호환성과 프록시 설정을 확인하세요.",
			"server_error":     "AI 서버가 모델 목록 조회 중 오류를 반환했습니다. 서버 상태를 확인하세요.",
			"request_rejected": "모델 목록 조회 요청이 거부되었습니다. Endpoint와 게이트웨이 정책을 확인하세요.",
		}[limits.Reason]
		if reason == "" {
			reason = "서버에서 모델 한도를 확인할 수 없습니다."
		}
		return reason + " " + fallback + " 모델 목록 조회 결과만으로 Chat·Embedding 연결 성공 여부를 판단하지 않습니다."
	}
}
