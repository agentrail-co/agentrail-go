package coexistence_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The caller's W3C trace.
const (
	incomingTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	incomingSpan  = "00f067aa0ba902b7"
)

var w3cIncoming = http.Header{
	"Traceparent": {"00-" + incomingTrace + "-" + incomingSpan + "-01"},
	"Tracestate":  {"vendor=incoming"},
	"Baggage":     {"tenant=synthetic"},
}

// beside runs one request through otelhttp's middleware and transport
// outside ours, the handler starting a span of the application's own, and
// returns what the application's OpenTelemetry recorded.
func beside(t *testing.T) (outcome, []tracetest.SpanStub) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { provider.Shutdown(context.Background()) })
	propagator := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	got := serve(t, w3cIncoming, &tracer{
		handler: func(h http.Handler) http.Handler {
			return otelhttp.NewHandler(h, "application.server", otelhttp.WithTracerProvider(provider), otelhttp.WithPropagators(propagator))
		},
		transport: func(rt http.RoundTripper) http.RoundTripper {
			return otelhttp.NewTransport(rt, otelhttp.WithTracerProvider(provider), otelhttp.WithPropagators(propagator))
		},
		work: func(ctx context.Context) (context.Context, func()) {
			ctx, work := provider.Tracer("application").Start(ctx, "application.work")
			return ctx, func() { work.End() }
		},
	})
	return got, exporter.GetSpans()
}

// This fails if someone clears or replaces OpenTelemetry's span in the
// request's context again, or strips the application's trace headers from
// its calls: the application's trace
// would stop at our middleware, its spans would hang off ours, or its next
// service would start a new trace.
func TestRunsBesideOpenTelemetry(t *testing.T) {
	got, theirs := beside(t)

	// (1) The application's trace went on through our capture.
	var server, work tracetest.SpanStub
	calls := map[string]tracetest.SpanStub{}
	for _, s := range theirs {
		switch {
		case s.SpanKind == trace.SpanKindServer:
			server = s
		case s.Name == "application.work":
			work = s
		case s.SpanKind == trace.SpanKindClient:
			calls[s.SpanContext.SpanID().String()] = s
		}
	}
	if server.SpanContext.TraceID().String() != incomingTrace || server.Parent.SpanID().String() != incomingSpan {
		t.Fatalf("the application's server span did not continue the caller's trace: %v", server)
	}
	if work.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Fatalf("the application's own span is not a child of its server span: %v", work.Parent)
	}
	if len(calls) != 2 {
		t.Fatalf("the application recorded %d calls", len(calls))
	}
	for _, s := range calls {
		if s.Parent.SpanID() != work.SpanContext.SpanID() {
			t.Fatalf("the application's call is not a child of its own span: %v", s.Parent)
		}
	}

	// (2) The ordinary origin got the application's headers and none of ours.
	traceID, parentID := traceparentOf(got.ordinary)
	if _, ok := calls[parentID]; traceID != incomingTrace || !ok {
		t.Fatalf("ordinary origin got traceparent %q", got.ordinary.Get("traceparent"))
	}
	if got.ordinary.Get("tracestate") != "vendor=incoming" || got.ordinary.Get("baggage") != "tenant=synthetic" {
		t.Fatalf("ordinary origin lost the application's headers: %v", got.ordinary)
	}
	// The allowed origin keeps the application's baggage beside our traceparent.
	if got.allowed.Get("baggage") != "tenant=synthetic" {
		t.Fatalf("allowed origin lost the application's baggage: %v", got.allowed)
	}

	// (3) Our headers and spans are as without OpenTelemetry.
	checkOurs(t, got, serve(t, w3cIncoming, nil))
	for _, s := range got.ours {
		if hex.EncodeToString(s.TraceId) == incomingTrace {
			t.Fatalf("our span joined the application's trace: %v", s)
		}
	}
}

// This fails if log correlation reads OpenTelemetry's current span: a log
// written inside the application's own span would name a trace Agentrail
// never receives.
func TestLogBesideOpenTelemetryIsCorrelatedToOurSpan(t *testing.T) {
	got, _ := beside(t)
	entry := ourSpan(t, got.ours, tracepb.Span_SPAN_KIND_SERVER, "")
	if len(got.logs) != 1 || !bytes.Equal(got.logs[0].TraceId, entry.TraceId) || !bytes.Equal(got.logs[0].SpanId, entry.SpanId) {
		t.Fatalf("log not correlated to our entry %x/%x: %v", entry.TraceId, entry.SpanId, got.logs)
	}
}
