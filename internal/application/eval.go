package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EvalCase is one labeled evaluation case: run analysisType over MessageID and
// check that the result (optionally a specific JSON field) contains Expected.
type EvalCase struct {
	MessageID string `json:"message_id"`
	Expected  string `json:"expected"`
	Field     string `json:"field,omitempty"` // JSON field to compare; empty = whole result
}

// EvalCaseResult is the per-case outcome.
type EvalCaseResult struct {
	MessageID string `json:"message_id"`
	Expected  string `json:"expected"`
	Got       string `json:"got"`
	Pass      bool   `json:"pass"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// EvalResult aggregates an evaluation run over the active prompt version.
type EvalResult struct {
	AnalysisType  string           `json:"analysis_type"`
	PromptVersion string           `json:"prompt_version"`
	Model         string           `json:"model"`
	Total         int              `json:"total"`
	Passed        int              `json:"passed"`
	Accuracy      float64          `json:"accuracy"`
	AvgLatencyMS  int64            `json:"avg_latency_ms"`
	Cases         []EvalCaseResult `json:"cases"`
}

// EvaluatePrompt runs an analysis type over labeled cases and reports accuracy
// and latency for the currently active prompt version (§AI 품질 평가 체계). It is
// a lightweight harness: cases are supplied inline, so no dataset store is
// required. Results reuse the analysis cache, so re-runs are cheap.
func (a *App) EvaluatePrompt(ctx context.Context, analysisType string, cases []EvalCase) (*EvalResult, error) {
	pv, _, ok := a.activePrompt(analysisType)
	if !ok {
		return nil, userErrf("unknown analysis type %q", analysisType)
	}
	if len(cases) == 0 {
		return nil, userErrf("no evaluation cases provided")
	}
	res := &EvalResult{AnalysisType: analysisType, PromptVersion: pv}
	var totalLatency int64
	for _, c := range cases {
		start := time.Now()
		an, err := a.AnalyzeMessage(ctx, c.MessageID, analysisType)
		latency := time.Since(start).Milliseconds()
		totalLatency += latency
		cr := EvalCaseResult{MessageID: c.MessageID, Expected: c.Expected, LatencyMS: latency}
		if err != nil {
			cr.Error = err.Error()
			res.Cases = append(res.Cases, cr)
			continue
		}
		if res.Model == "" {
			res.Model = an.Model
		}
		got := extractEvalField(an.ResultJSON, c.Field)
		cr.Got = got
		cr.Pass = strings.Contains(strings.ToLower(got), strings.ToLower(strings.TrimSpace(c.Expected)))
		if cr.Pass {
			res.Passed++
		}
		res.Cases = append(res.Cases, cr)
	}
	res.Total = len(cases)
	if res.Total > 0 {
		res.Accuracy = float64(res.Passed) / float64(res.Total)
		res.AvgLatencyMS = totalLatency / int64(res.Total)
	}
	a.audit(ctx, "prompt_eval", "analysis:"+analysisType, "ok",
		fmt.Sprintf("acc=%.2f n=%d pv=%s", res.Accuracy, res.Total, pv))
	return res, nil
}

// extractEvalField returns the named JSON field's value as a string, or the
// whole payload when field is empty or missing.
func extractEvalField(resultJSON, field string) string {
	if field == "" {
		return resultJSON
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(resultJSON), &m); err != nil {
		return resultJSON
	}
	v, ok := m[field]
	if !ok {
		return resultJSON
	}
	switch x := v.(type) {
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
