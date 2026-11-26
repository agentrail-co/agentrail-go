package coexistence_test

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/handoff"
	agentrailslog "github.com/agentrail-co/agentrail-go/slog"
	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const hashKey = "synthetic-workspace-key"

// tracer is another tracer around Agentrail, as the application set it up.
type tracer struct {
	// handler and transport wrap our middleware and our transport.
	handler   func(http.Handler) http.Handler
	transport func(http.RoundTripper) http.RoundTripper
	// work, when set, runs first in the handler and returns the context the
	// handler logs and calls with, and a function to end it.
	work func(context.Context) (context.Context, func())
}

// outcome is what one request left behind: the spans and logs our collector
// received, and the headers each origin the handler called received.
type outcome struct {
	ours              []*tracepb.Span
	logs              []*logpb.LogRecord
	ordinary, allowed http.Header
}

// serve sends one request with incoming headers through our middleware,
// inside other's (none when other is nil). Its handler writes a slog log and
// calls an ordinary origin and an allowed one through our transport, each
// call carrying an x-agentrail-cli header the application forwards, which is
// Agentrail's to take off.
func serve(t *testing.T, incoming http.Header, other *tracer) outcome {
	t.Helper()
	var mu sync.Mutex
	var out outcome
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		raw, err := io.ReadAll(q.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(q.URL.Path, "/traces"):
			export := new(collectortrace.ExportTraceServiceRequest)
			if err := proto.Unmarshal(raw, export); err != nil {
				t.Error(err)
				return
			}
			for _, rs := range export.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					out.ours = append(out.ours, ss.Spans...)
				}
			}
		case strings.HasSuffix(q.URL.Path, "/logs"):
			export := new(collectorlog.ExportLogsServiceRequest)
			if err := proto.Unmarshal(raw, export); err != nil {
				t.Error(err)
				return
			}
			for _, rl := range export.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					out.logs = append(out.logs, sl.LogRecords...)
				}
			}
		}
	}))
	defer collector.Close()
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
	c, err := agentrail.New(context.Background(), agentrail.Options{
		Collector: agentrail.CollectorOptions{Endpoint: collector.URL + "/ingest/otlp", Secret: "synthetic-source"},
		HashKey:   hashKey, ServiceName: "coexistence-test", OutboundOrigins: []string{allowed.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	logger := slog.New(agentrailslog.NewHandler(c, slog.NewTextHandler(io.Discard, nil)))
	transport := c.Transport(nil)
	if other != nil {
		transport = other.transport(transport)
	}
	client := &http.Client{Transport: transport}
	handler := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		ctx := q.Context()
		if other != nil && other.work != nil {
			var end func()
			ctx, end = other.work(ctx)
			defer end()
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
	if other != nil {
		handler = other.handler(handler)
	}
	in := httptest.NewRequest(http.MethodGet, "http://app.test/orders", nil)
	for k, vs := range incoming {
		in.Header[k] = vs
	}
	handler.ServeHTTP(httptest.NewRecorder(), in)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	out.ordinary, out.allowed = received["/ordinary"], received["/allowed"]
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

// ourSpan is our span of kind whose URL ends with suffix, or fails.
func ourSpan(t *testing.T, all []*tracepb.Span, kind tracepb.Span_SpanKind, suffix string) *tracepb.Span {
	t.Helper()
	for _, s := range all {
		if s.Kind == kind && strings.HasSuffix(urlOf(s), suffix) {
			return s
		}
	}
	t.Fatalf("our spans did not arrive: %v", shape(all))
	return nil
}

// checkOurs fails unless the ordinary origin got none of Agentrail's headers,
// the allowed origin got our signed traceparent and handoff and neither the
// application's tracestate nor x-agentrail-cli, and our collector received
// the same spans as baseline, the same request with no other tracer.
func checkOurs(t *testing.T, got, baseline outcome) {
	t.Helper()
	if got.ordinary.Get("x-agentrail-trace-handoff") != "" || got.ordinary.Get("x-agentrail-cli") != "" {
		t.Errorf("ordinary origin got Agentrail's headers: %v", got.ordinary)
	}
	entry := ourSpan(t, got.ours, tracepb.Span_SPAN_KIND_SERVER, "")
	call := ourSpan(t, got.ours, tracepb.Span_SPAN_KIND_CLIENT, "/allowed")
	want := "00-" + hex.EncodeToString(entry.TraceId) + "-" + hex.EncodeToString(call.SpanId) + "-01"
	if got.allowed.Get("traceparent") != want {
		t.Errorf("allowed origin got traceparent %q, want ours %q", got.allowed.Get("traceparent"), want)
	}
	if _, err := handoff.Verify(hashKey, got.allowed.Get("x-agentrail-trace-handoff"), want, http.MethodGet, "/allowed", time.Now().UnixMilli()); err != nil {
		t.Errorf("allowed origin's handoff did not verify: %v", err)
	}
	if got.allowed.Get("tracestate") != "" || got.allowed.Get("x-agentrail-cli") != "" {
		t.Errorf("allowed origin got %v", got.allowed)
	}
	if a, b := shape(got.ours), shape(baseline.ours); !slices.Equal(a, b) {
		t.Errorf("our spans beside the other tracer:\n%v\nwithout it:\n%v", a, b)
	}
}

func urlOf(s *tracepb.Span) string {
	for _, a := range s.Attributes {
		if a.Key == "url.full" {
			return a.Value.GetStringValue()
		}
	}
	return ""
}

// traceparentOf is a traceparent header's trace ID and parent span ID.
func traceparentOf(h http.Header) (string, string) {
	parts := strings.Split(h.Get("traceparent"), "-")
	if len(parts) != 4 {
		return "", ""
	}
	return parts[1], parts[2]
}
