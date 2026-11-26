package coexistence_test

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/handoff"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const hashKey = "synthetic-workspace-key"

// outcome is what one request left behind: the spans our collector received,
// and the headers each origin the handler called received.
type outcome struct {
	ours              []*tracepb.Span
	ordinary, allowed http.Header
}

// unchanged wraps nothing: the baseline run, with no other tracer.
func unchanged[T any](v T) T { return v }

// serve sends one request with incoming headers through wrap(our middleware).
// Its handler calls an ordinary origin and an allowed one through
// around(our transport), each call carrying an x-agentrail-cli header the
// application forwards, which is Agentrail's to take off.
func serve(t *testing.T, incoming http.Header, wrap func(http.Handler) http.Handler, around func(http.RoundTripper) http.RoundTripper) outcome {
	t.Helper()
	var mu sync.Mutex
	var ours []*tracepb.Span
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		raw, err := io.ReadAll(q.Body)
		if err != nil || !strings.HasSuffix(q.URL.Path, "/traces") {
			return
		}
		export := new(collectortrace.ExportTraceServiceRequest)
		if err := proto.Unmarshal(raw, export); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, rs := range export.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				ours = append(ours, ss.Spans...)
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
	client := &http.Client{Transport: around(c.Transport(nil))}
	handler := wrap(c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		for _, url := range []string{ordinary.URL + "/ordinary", allowed.URL + "/allowed"} {
			call, err := http.NewRequestWithContext(q.Context(), http.MethodGet, url, nil)
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
	})))
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
	return outcome{ours: ours, ordinary: received["/ordinary"], allowed: received["/allowed"]}
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

// checkOurs fails unless the ordinary origin got none of Agentrail's headers,
// the allowed origin got our signed traceparent and handoff and neither the
// application's tracestate nor x-agentrail-cli, and our collector received
// the same spans as baseline, the same request with no other tracer.
func checkOurs(t *testing.T, got, baseline outcome) {
	t.Helper()
	if got.ordinary.Get("x-agentrail-trace-handoff") != "" || got.ordinary.Get("x-agentrail-cli") != "" {
		t.Errorf("ordinary origin got Agentrail's headers: %v", got.ordinary)
	}
	var entry, call *tracepb.Span
	for _, s := range got.ours {
		switch {
		case s.Kind == tracepb.Span_SPAN_KIND_SERVER:
			entry = s
		case s.Kind == tracepb.Span_SPAN_KIND_CLIENT && strings.HasSuffix(urlOf(s), "/allowed"):
			call = s
		}
	}
	if entry == nil || call == nil {
		t.Fatalf("our spans did not arrive: %v", shape(got.ours))
	}
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
