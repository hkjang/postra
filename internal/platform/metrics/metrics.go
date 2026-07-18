// Package metrics defines the Prometheus collectors exposed at /metrics (§18.1).
//
// Collectors are package-level singletons registered to a private registry, so
// instrumentation call sites stay a one-liner (metrics.SyncTotal.WithLabelValues
// ("succeeded").Inc()) and no metrics handle has to be threaded through App.
// Label sets are deliberately low-cardinality — no per-account or per-message
// labels — so the series count stays bounded on long-running servers.
package metrics

import (
	"net/http"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"postra/internal/platform/build"
)

const namespace = "postra"

var reg = prometheus.NewRegistry()

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: name, Help: help,
	}, labels)
	reg.MustRegister(c)
	return c
}

func plainCounter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: name, Help: help,
	})
	reg.MustRegister(c)
	return c
}

func gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: name, Help: help,
	})
	reg.MustRegister(g)
	return g
}

func histogram(name, help string, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: name, Help: help,
		Buckets: prometheus.DefBuckets,
	}, labels)
	reg.MustRegister(h)
	return h
}

var (
	// POP3 ingest.
	SyncTotal       = counter("pop3_sync_total", "POP3 sync jobs by terminal status.", "status")
	MessagesFetched = plainCounter("pop3_messages_fetched_total", "New messages ingested via POP3.")

	// AI provider.
	AIRequests = counter("ai_requests_total", "AI provider calls by op and result.", "op", "result")
	AILatency  = histogram("ai_request_duration_seconds", "AI provider call latency.", "op")

	// SMTP send.
	SMTPSend    = counter("smtp_send_total", "Outbound deliveries by result.", "result")
	SMTPRetries = plainCounter("smtp_retry_total", "Outbox retry attempts processed by the worker.")

	// MCP tools.
	MCPRequests = counter("mcp_requests_total", "MCP tool invocations by tool and result.", "tool", "result")

	// REST transport.
	HTTPRequests = counter("http_requests_total", "REST requests by route, method and status code.", "route", "method", "code")
	HTTPLatency  = histogram("http_request_duration_seconds", "REST request latency by route.", "route")

	// Queue depth.
	OutboxPending = gauge("outbox_pending", "Outbound messages awaiting a (re)delivery attempt.")
)

func init() {
	// Standard process/Go runtime metrics alongside the app's own.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	// build_info exposes the release and Go version as a constant "1" series,
	// so dashboards can join metrics to the deployed version.
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info", Help: "Build metadata; value is always 1.",
	}, []string{"version", "goversion"})
	reg.MustRegister(info)
	info.WithLabelValues(build.Version, runtime.Version()).Set(1)
}

// Handler serves the Prometheus text exposition for the registered collectors.
func Handler() http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
