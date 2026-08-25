package application

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"postra/internal/domain"
	"postra/internal/platform/metrics"
)

// Prompt-injection heuristics. Postra already keeps mail content strictly
// inside the untrusted data block (AI-014), so a crafted mail cannot rewrite
// the instruction channel — but content that *tries* is a strong signal the
// message is hostile, and the user deserves to know before acting on any
// AI output derived from it. These patterns therefore FLAG, never block:
// false positives must not break legitimate mail.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bignore\s+(all\s+|any\s+)?(the\s+)?(previous|prior|above|earlier)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)\bdisregard\s+(all\s+|any\s+)?(the\s+)?(previous|prior|above|earlier)\b`),
	regexp.MustCompile(`(?i)\bforget\s+(everything|all\s+previous|your\s+instructions)`),
	regexp.MustCompile(`(?i)\b(you\s+are\s+now|from\s+now\s+on\s+you)\b`),
	regexp.MustCompile(`(?i)\b(system|developer)\s*(prompt|message|instruction)s?\b`),
	// (?m) is essential: Go anchors ^ to the start of the text unless it is
	// set, so without it this rule only fired when the entire mail began with
	// the marker — never the realistic case of an injected line mid-body.
	regexp.MustCompile(`(?im)^\s*(system|assistant)\s*:`),
	regexp.MustCompile(`(?i)\b(reveal|print|repeat|show)\s+(your|the)\s+(system\s+)?(prompt|instructions?|rules?)`),
	regexp.MustCompile(`(?i)\bact\s+as\s+(an?\s+)?(unrestricted|different|new)\b`),
	regexp.MustCompile(`(?i)\b(jailbreak|DAN\s+mode|developer\s+mode)\b`),
	// Korean equivalents.
	regexp.MustCompile(`(이전|위의?|앞의)\s*(지시|명령|지침|프롬프트)[^\n]{0,12}(무시|잊)`),
	regexp.MustCompile(`시스템\s*프롬프트`),
	regexp.MustCompile(`너는\s*이제|당신은\s*이제`),
}

// detectPromptInjection returns the patterns a piece of untrusted content
// matched. An empty result means nothing suspicious was found.
func detectPromptInjection(content string) []string {
	if content == "" {
		return nil
	}
	var hits []string
	for _, re := range injectionPatterns {
		if m := re.FindString(content); m != "" {
			hits = append(hits, strings.TrimSpace(truncateRunes(m, 80)))
		}
	}
	return hits
}

// guardUntrusted screens mail content headed for the model. Detections are
// audited, counted, and recorded as an incident so admins can see hostile mail
// in the 오류/장애 dashboard; the analysis itself proceeds (the content stays
// confined to the data block either way).
func (a *App) guardUntrusted(ctx context.Context, analysisType, targetType, targetID, content string) []string {
	hits := detectPromptInjection(content)
	if len(hits) == 0 {
		return nil
	}
	metrics.AIInjectionFlags.Inc()
	detail := fmt.Sprintf("analysis=%s target=%s:%s patterns=%v", analysisType, targetType, targetID, hits)
	slog.Warn("prompt-injection attempt detected in mail content", "analysis", analysisType, "target", targetID, "patterns", hits)
	a.audit(ctx, "ai_prompt_injection_detected", targetType+":"+targetID, "ok", detail)
	a.recordIncident(domain.SeverityWarning, "ai-injection",
		"메일 본문에서 AI 조작(프롬프트 인젝션) 시도가 감지되었습니다", detail)
	return hits
}

// verifyCitations strips evidence message IDs the model did not actually get.
// The Q&A prompt asks for evidence_message_ids, but a model can invent or
// mangle them; an unverifiable citation is worse than none because it looks
// authoritative. Only IDs present in the retrieved context survive. Returns the
// corrected JSON and how many citations were dropped.
func verifyCitations(resultJSON string, allowed map[string]bool) (string, int) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(resultJSON), &obj); err != nil {
		return resultJSON, 0
	}
	// Models commonly collapse a single-element list into a bare string. Left
	// unhandled, that form skipped verification entirely and a fabricated
	// citation reached the user unchecked — the exact failure this guards.
	var raw []any
	normalized := false
	switch v := obj["evidence_message_ids"].(type) {
	case []any:
		raw = v
	case string:
		raw, normalized = []any{v}, true
	default:
		return resultJSON, 0
	}
	kept := []any{}
	dropped := 0
	for _, v := range raw {
		if id, ok := v.(string); ok && allowed[strings.TrimSpace(id)] {
			kept = append(kept, strings.TrimSpace(id))
			continue
		}
		dropped++
	}
	if dropped == 0 && !normalized {
		return resultJSON, 0
	}
	// Rewrite even when nothing was dropped, so a bare string always reaches
	// consumers as the list the schema promises.
	obj["evidence_message_ids"] = kept
	obj["citations_dropped"] = dropped
	if len(kept) == 0 {
		obj["citation_warning"] = "모델이 제시한 근거 메일 ID가 실제 검색 결과에 없어 모두 제거되었습니다. 답변의 근거를 신뢰하기 어렵습니다."
	}
	fixed, err := json.Marshal(obj)
	if err != nil {
		return resultJSON, 0
	}
	return string(fixed), dropped
}

// applyCitationVerification post-processes a question_answer result so callers
// never see citations that were not in the evidence actually supplied.
func (a *App) applyCitationVerification(ctx context.Context, an *domain.Analysis, allowed map[string]bool) {
	fixed, dropped := verifyCitations(an.ResultJSON, allowed)
	if dropped == 0 {
		return
	}
	an.ResultJSON = fixed
	slog.Warn("dropped unverifiable AI citations", "analysis", an.ID, "dropped", dropped)
	a.audit(ctx, "ai_citations_dropped", "analysis:"+an.ID, "ok",
		fmt.Sprintf("dropped=%d (모델이 근거로 제시한 메일 ID가 검색 결과에 없음)", dropped))
	metrics.AICitationsDropped.Add(float64(dropped))
}

var _ = context.Background
