package application

import (
	"context"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/metrics"
)

// meteredAI wraps an AIProvider to record call count, result, and latency
// centrally, so every transport that reaches the AI is measured the same way
// (§18.1). It is transparent — behaviour is identical to the wrapped provider.
type meteredAI struct{ inner domain.AIProvider }

func (m meteredAI) Generate(ctx context.Context, req domain.GenerationRequest) (domain.GenerationResult, error) {
	start := time.Now()
	res, err := m.inner.Generate(ctx, req)
	observeAI("generate", start, err)
	return res, err
}

func (m meteredAI) Embed(ctx context.Context, req domain.EmbeddingRequest) (domain.EmbeddingResult, error) {
	start := time.Now()
	res, err := m.inner.Embed(ctx, req)
	observeAI("embed", start, err)
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
