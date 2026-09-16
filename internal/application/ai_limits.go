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
	limits := domain.AIModelLimits{Model: route.Model, ContextLength: cfg.ContextLength, MaxOutputTokens: route.MaxTokens, Source: "config", Status: "unavailable"}
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
	}
	return limits, nil
}

func modelLimitsDiagnostic(limits domain.AIModelLimits) string {
	if limits.ContextLength == 0 && limits.MaxOutputTokens == 0 {
		return "임베딩 모델의 입력 한도는 확인되지 않았습니다. Chat Context 설정을 임베딩 모델에 대신 적용하지 않으며, 서버가 제공하는 한도만 검사합니다."
	}
	switch limits.Status {
	case "detected":
		return "선택한 모델의 한도를 확인했습니다. 요청 출력은 설정 상한과 남은 Context에 맞춰 자동 조정됩니다."
	case "manual":
		return "자동 감지가 꺼져 있어 수동 Context 한도를 적용합니다. AI 생성 요청은 보내지 않았습니다."
	case "model_not_found":
		return "모델 목록에서 선택한 ID를 찾지 못해 설정된 대체 한도를 사용합니다. 모델 이름을 확인하세요."
	default:
		return "서버에서 모델 한도를 확인할 수 없어 설정된 대체 한도를 사용합니다. 한도 조회 미지원은 AI 연결 실패를 의미하지 않습니다."
	}
}
