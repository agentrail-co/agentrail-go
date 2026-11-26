package agentrail_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/handoff"
	agentrailslog "github.com/agentrail-co/agentrail-go/slog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The incoming request's trace, as the application's caller sent it.
const (
	incomingTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	incomingSpan  = "00f067aa0ba902b7"
)

// application is a server's own OpenTelemetry: a provider exporting to
// memory, W3C trace context and baggage, otelhttp on both sides.
type application struct {
	exporter   *tracetest.InMemoryExporter
	provider   *sdktrace.TracerProvider
	propagator propagation.TextMapPropagator
}

func newApplication(t *testing.T) *application {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { provider.Shutdown(context.Background()) })
	return &application{exporter, provider, propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})}
}

// outcome is what one request left behind: our collector's spans and logs,
// and the headers each origin the handler called received.
type outcome struct {
	ours              []*tracepb.Span
	logs              []*logpb.LogRecord
	ordinary, allowed http.Header
}

// serve sends one request carrying the caller's trace headers through our
// middleware. Its handler writes a slog log and calls an ordinary origin and
// an allowed one through our transport. With app, otelhttp's middleware and
// transport sit outside ours, and the handler starts a span of its own.
func serve(t *testing.T, app *application) outcome {
	t.Helper()
	r, s := collector(t)
	var mu sync.Mutex
	received := map[string]http.Header{}
	record := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		received[q.URL.Path] = q.Header.Clone()
	})
	ordinary := httptest.NewServer(record)
	defer ordinary.Close()
	allowed := httptest.NewServer(record)
	defer allowed.Close()
	o := options(s.URL)
	o.OutboundOrigins = []string{allowed.URL}
	c, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	logger := slog.New(agentrailslog.NewHandler(c, slog.NewTextHandler(io.Discard, nil)))
	transport := c.Transport(nil)
	if app != nil {
		transport = otelhttp.NewTransport(transport, otelhttp.WithTracerProvider(app.provider), otelhttp.WithPropagators(app.propagator))
	}
	client := &http.Client{Transport: transport}
	var handler http.Handler = c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		ctx := q.Context()
		if app != nil {
			var work trace.Span
			ctx, work = app.provider.Tracer("application").Start(ctx, "application.work")
			defer work.End()
		}
		logger.InfoContext(ctx, "handled", "event.name", "request.handled")
		for _, url := range []string{ordinary.URL + "/ordinary", allowed.URL + "/allowed"} {
			call, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				t.Error(err)
				return
			}
			call.Header.Set("x-agentrail-cli", "forwarded-by-the-application")
			response, err := client.Do(call)
			if err != nil {
				t.Error(err)
				return
			}
			response.Body.Close()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if app != nil {
		handler = otelhttp.NewHandler(handler, "application.server", otelhttp.WithTracerProvider(app.provider), otelhttp.WithPropagators(app.propagator))
	}
	in := httptest.NewRequest(http.MethodGet, "http://app.test/orders", nil)
	in.Header.Set("traceparent", "00-"+incomingTrace+"-"+incomingSpan+"-01")
	in.Header.Set("tracestate", "vendor=incoming")
	in.Header.Set("baggage", "tenant=synthetic")
	handler.ServeHTTP(httptest.NewRecorder(), in)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := outcome{ours: spans(r), ordinary: received["/ordinary"], allowed: received["/allowed"]}
	for _, packet := range r.logs {
		for _, rl := range packet.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				out.logs = append(out.logs, sl.LogRecords...)
			}
		}
	}
	return out
}

// shape is our spans without IDs, times or ports: name, kind, the parent's
// name, link count and attribute names, sorted.
func shape(all []*tracepb.Span) []string {
	byID := map[string]*tracepb.Span{}
	for _, s := range all {
		byID[string(s.SpanId)] = s
	}
	out := []string{}
	for _, s := range all {
		parent := ""
		if p, ok := byID[string(s.ParentSpanId)]; ok {
			parent = p.Name
		} else if len(s.ParentSpanId) > 0 {
			parent = "elsewhere"
		}
		keys := []string{}
		for _, a := range s.Attributes {
			keys = append(keys, a.Key)
		}
		slices.Sort(keys)
		out = append(out, strings.Join([]string{s.Name, s.Kind.String(), parent, string(rune('0' + len(s.Links))), strings.Join(keys, ",")}, "|"))
	}
	slices.Sort(out)
	return out
}

func ofKind(t *testing.T, all []*tracepb.Span, kind tracepb.Span_SpanKind, url string) *tracepb.Span {
	t.Helper()
	for _, s := range all {
		if s.Kind == kind && (url == "" || strings.HasSuffix(attr(s.Attributes, "url.full").GetStringValue(), url)) {
			return s
		}
	}
	t.Fatalf("no %v span %s in %v", kind, url, shape(all))
	return nil
}

// traceparentOf is a traceparent's trace ID and parent span ID.
func traceparentOf(h http.Header) (string, string) {
	parts := strings.Split(h.Get("traceparent"), "-")
	if len(parts) != 4 {
		return "", ""
	}
	return parts[1], parts[2]
}

// This fails if someone clears or replaces OpenTelemetry's span in the
// request's context again, or strips the application's trace headers from
// its calls: the application's trace would stop at our middleware, its
// spans would hang off ours, or its next service would start a new trace.
func TestRunsBesideOpenTelemetry(t *testing.T) {
	app := newApplication(t)
	got := serve(t, app)

	// (1) The application's trace went on through our capture.
	theirs := app.exporter.GetSpans()
	var server, work tracetest.SpanStub
	clients := map[string]tracetest.SpanStub{}
	for _, s := range theirs {
		switch {
		case s.SpanKind == trace.SpanKindServer:
			server = s
		case s.Name == "application.work":
			work = s
		case s.SpanKind == trace.SpanKindClient:
			clients[s.SpanContext.SpanID().String()] = s
		}
	}
	if server.SpanContext.TraceID().String() != incomingTrace || server.Parent.SpanID().String() != incomingSpan {
		t.Fatalf("the application's server span did not continue the caller's trace: %v", server)
	}
	if work.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Fatalf("the application's own span is not a child of its server span: %v", work.Parent)
	}
	if len(clients) != 2 {
		t.Fatalf("the application recorded %d calls", len(clients))
	}
	for _, s := range clients {
		if s.Parent.SpanID() != work.SpanContext.SpanID() {
			t.Fatalf("the application's call is not a child of its own span: %v", s.Parent)
		}
	}

	// (2) The ordinary origin got the application's headers and none of ours.
	traceID, parentID := traceparentOf(got.ordinary)
	if _, ok := clients[parentID]; traceID != incomingTrace || !ok {
		t.Fatalf("ordinary origin got traceparent %q", got.ordinary.Get("traceparent"))
	}
	if got.ordinary.Get("tracestate") != "vendor=incoming" || got.ordinary.Get("baggage") != "tenant=synthetic" {
		t.Fatalf("ordinary origin lost the application's headers: %v", got.ordinary)
	}
	if got.ordinary.Get("x-agentrail-trace-handoff") != "" || got.ordinary.Get("x-agentrail-cli") != "" {
		t.Fatalf("ordinary origin got Agentrail's headers: %v", got.ordinary)
	}
	// The allowed origin got our signed traceparent in place of theirs.
	entry := ofKind(t, got.ours, tracepb.Span_SPAN_KIND_SERVER, "")
	call := ofKind(t, got.ours, tracepb.Span_SPAN_KIND_CLIENT, "/allowed")
	traceID, parentID = traceparentOf(got.allowed)
	if traceID != hex.EncodeToString(entry.TraceId) || parentID != hex.EncodeToString(call.SpanId) {
		t.Fatalf("allowed origin got traceparent %q", got.allowed.Get("traceparent"))
	}
	if _, err := handoff.Verify(options("").HashKey, got.allowed.Get("x-agentrail-trace-handoff"), got.allowed.Get("traceparent"), http.MethodGet, "/allowed", time.Now().UnixMilli()); err != nil {
		t.Fatalf("allowed origin's handoff did not verify: %v", err)
	}
	if got.allowed.Get("tracestate") != "" || got.allowed.Get("x-agentrail-cli") != "" {
		t.Fatalf("allowed origin got %v", got.allowed)
	}

	// (3) Our collector got the same spans as without the application's tracing.
	if want := shape(serve(t, nil).ours); !reflect.DeepEqual(shape(got.ours), want) {
		t.Fatalf("our spans beside OpenTelemetry:\n%v\nwithout it:\n%v", shape(got.ours), want)
	}
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
	app := newApplication(t)
	got := serve(t, app)
	entry := ofKind(t, got.ours, tracepb.Span_SPAN_KIND_SERVER, "")
	if len(got.logs) != 1 || !bytes.Equal(got.logs[0].TraceId, entry.TraceId) || !bytes.Equal(got.logs[0].SpanId, entry.SpanId) {
		t.Fatalf("log not correlated to our entry %x/%x: %v", entry.TraceId, entry.SpanId, got.logs)
	}
}

// This fails if a milestone outside a request takes the application's span
// out of the context its work runs in, or stops linking to it.
func TestMilestoneOutsideARequestKeepsTheApplicationsSpan(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	app := newApplication(t)
	ctx, job := app.provider.Tracer("application").Start(context.Background(), "application.job")
	err = c.WithMilestone(ctx, "nightly.sync", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "sync"}, func(ctx context.Context) error {
		if trace.SpanFromContext(ctx) != job {
			t.Error("the application's span is no longer current inside the milestone")
		}
		_, step := app.provider.Tracer("application").Start(ctx, "application.step")
		step.End()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	job.End()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range app.exporter.GetSpans() {
		if s.Name == "application.step" && s.Parent.SpanID() != job.SpanContext().SpanID() {
			t.Fatalf("the application's step is not a child of its job: %v", s.Parent)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ours := spans(r)
	jobID := job.SpanContext().SpanID()
	if len(ours) != 1 || len(ours[0].ParentSpanId) != 0 || len(ours[0].Links) != 1 || !bytes.Equal(ours[0].Links[0].SpanId, jobID[:]) {
		t.Fatalf("milestone is not a root linked to the application's span: %v", ours)
	}
}
