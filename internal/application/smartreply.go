package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ReplySuggestions is a small set of ready-to-send reply options generated for
// a received message (Gmail-style Smart Reply). Suggestions are the user's to
// pick, edit, and send — nothing is sent automatically (§1 발송 원칙).
type ReplySuggestions struct {
	Suggestions []string `json:"suggestions"`
	Model       string   `json:"model,omitempty"`
}

// SuggestReplies asks the AI for short, context-aware reply options for a
// message. It reuses the analysis pipeline, so results are PII-masked before
// leaving the box (AI-011), versioned/cached (AI-008/013), and produced from a
// strict instruction/data-separated prompt (AI-014) — the message body only
// ever enters the untrusted block.
func (a *App) SuggestReplies(ctx context.Context, messageID string) (*ReplySuggestions, error) {
	if err := a.checkAIPolicy(ctx); err != nil {
		return nil, err
	}
	_, input, err := a.messageAsAIInput(ctx, messageID, false)
	if err != nil {
		return nil, err
	}
	an, err := a.runAnalysis(ctx, "smart_reply", "message", messageID, "", input)
	if err != nil {
		return nil, err
	}
	var out ReplySuggestions
	if err := json.Unmarshal([]byte(an.ResultJSON), &out); err != nil {
		return nil, fmt.Errorf("smart reply output was not valid JSON: %w", err)
	}
	cleaned := make([]string, 0, len(out.Suggestions))
	for _, s := range out.Suggestions {
		if s = strings.TrimSpace(s); s != "" {
			cleaned = append(cleaned, truncateRunes(s, 2000))
		}
	}
	if len(cleaned) == 0 {
		return nil, userErrf("AI가 답장 제안을 생성하지 못했습니다")
	}
	out.Suggestions = cleaned
	out.Model = an.Model
	return &out, nil
}
