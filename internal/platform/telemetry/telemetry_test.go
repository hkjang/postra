package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type unprintableProviderError struct{}

func (unprintableProviderError) Error() string {
	panic("telemetry must not format arbitrary provider errors")
}

func TestEndExportsOnlyFixedErrorDiagnostics(t *testing.T) {
	const secret = "unpatterned-private-provider-password-581"
	for _, test := range []struct {
		name string
		err  error
		kind string
	}{
		{"provider", errors.New(secret), "operation_failed"},
		{"unprintable", unprintableProviderError{}, "operation_failed"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"success", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			_, span := provider.Tracer("test-local-no-exporter").Start(context.Background(), "ai.generate")
			End(span, test.err)
			spans := recorder.Ended()
			if len(spans) != 1 {
				t.Fatal("span not ended")
			}
			wire, err := json.Marshal(struct {
				Status     any
				Events     any
				Attributes any
			}{spans[0].Status(), spans[0].Events(), spans[0].Attributes()})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), secret) {
				t.Fatal("OTLP span payload leaked raw provider credentials")
			}
			if test.err == nil {
				if spans[0].Status().Code == codes.Error || len(spans[0].Events()) != 0 {
					t.Fatal("successful span reported failure")
				}
				return
			}
			if spans[0].Status().Code != codes.Error || len(spans[0].Events()) != 1 {
				t.Fatal("sanitizing diagnostics lost failure status/event")
			}
			found := false
			for _, attr := range spans[0].Attributes() {
				if attr.Key == "error.type" && attr.Value.AsString() == test.kind {
					found = true
				}
			}
			if !found {
				t.Fatal("safe error classification missing")
			}
		})
	}
}
