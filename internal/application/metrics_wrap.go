package application

import (
	"context"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/metrics"
	"postra/internal/platform/telemetry"
)

// meteredAI wraps an AIProvider to record call count, result, and latency
// centrally, so every transport that reaches the AI is measured the same way
// (§18.1). It is transparent — behaviour is identical to the wrapped provider.
type meteredAI struct {
	inner domain.AIProvider
	// rates returns the current USD-per-million input/output token prices.
	// Read at record time so a settings change takes effect without a restart.
	rates func() (in, out float64)
}

func (m meteredAI) Generate(ctx context.Context, req domain.GenerationRequest) (domain.GenerationResult, error) {
	ctx, span := telemetry.Start(ctx, "ai.generate")
	if req.Task != "" {
		span.SetAttributes(telemetry.Attr("ai.task", req.Task))
	}
	start := time.Now()
	res, err := m.inner.Generate(ctx, req)
	observeAI("generate", start, err)
	m.observeUsage("generate", res.Usage)
	telemetry.End(span, err)
	return res, err
}

func (m meteredAI) Embed(ctx context.Context, req domain.EmbeddingRequest) (domain.EmbeddingResult, error) {
	ctx, span := telemetry.Start(ctx, "ai.embed")
	start := time.Now()
	res, err := m.inner.Embed(ctx, req)
	observeAI("embed", start, err)
	m.observeUsage("embed", res.Usage)
	telemetry.End(span, err)
	return res, err
}

func observeAI(op string, start time.Time, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	metrics.AIRequests.WithLabelValues(op, result).Inc()
	metrics.AILatency.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

// observeUsage records token consumption and its estimated cost. Providers
// that report no usage (zero) are skipped rather than counted as free.
func (m meteredAI) observeUsage(op string, u domain.TokenUsage) {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return
	}
	if u.PromptTokens > 0 {
		metrics.AITokens.WithLabelValues(op, "input").Add(float64(u.PromptTokens))
	}
	if u.CompletionTokens > 0 {
		metrics.AITokens.WithLabelValues(op, "output").Add(float64(u.CompletionTokens))
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens > 0 {
		// Only when the provider gives no breakdown. Emitting a "total" series
		// alongside input/output would make sum(ai_tokens_total) count every
		// token twice.
		metrics.AITokens.WithLabelValues(op, "total").Add(float64(u.TotalTokens))
	}
	if m.rates == nil {
		return
	}
	inRate, outRate := m.rates()
	cost := float64(u.PromptTokens)/1e6*inRate + float64(u.CompletionTokens)/1e6*outRate
	if cost > 0 {
		metrics.AICostUSD.WithLabelValues(op).Add(cost)
	}
}
