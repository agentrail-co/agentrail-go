package agentrail_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	"go.opentelemetry.io/otel/trace"
)

// This fails if a host that only starts with the collector's is taken for
// the collector: the call would lose the application's trace headers, which
// only a call to the collector itself does.
func TestCollectorHostnamePrefixIsNotTheCollector(t *testing.T) {
	var received http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received = r.Header.Clone() }))
	defer target.Close()
	o := options("http://collector.example")
	o.Collector.Endpoint = "http://collector.example"
	o.OnError = func(context.Context, agentrail.CaptureError) {}
	c, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Shutdown(ctx)
	base := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target.Listener.Addr().String())
	}}
	defer base.CloseIdleConnections()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := http.NewRequestWithContext(r.Context(), "GET", "http://collector.example.attacker.test/v1/traces", nil)
		q.Header.Set("baggage", "private-baggage")
		q.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
		resp, err := c.Transport(base).RoundTrip(q)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}))
	q := httptest.NewRequest("GET", "http://app.test/", nil)
	h.ServeHTTP(httptest.NewRecorder(), q)
	if received.Get("baggage") != "private-baggage" || received.Get("traceparent") != "00-11111111111111111111111111111111-2222222222222222-01" {
		t.Fatalf("a collector hostname prefix was treated as the collector: %v", received)
	}
	cancel()
}
func TestStandaloneMilestoneRetainsExternalDirectionalLink(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	tid, _ := trace.TraceIDFromHex("11111111111111111111111111111111")
	sid, _ := trace.SpanIDFromHex("2222222222222222")
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, Remote: true}))
	c.Milestone(ctx, "standalone", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "operation"})
	c.Flush(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	ss := spans(r)
	if len(ss) != 1 || len(ss[0].Links) != 1 || len(ss[0].ParentSpanId) != 0 {
		t.Fatal("lost directional link or trusted external parent")
	}
}

// This fails if a call the application makes to the collector's own
// ingestion path keeps any trace header, another tracer's or Agentrail's, or
// is recorded as a client span of the request.
func TestCollectorCallsCarryNoTraceHeaders(t *testing.T) {
	var mu sync.Mutex
	var received []http.Header
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == http.MethodGet {
			mu.Lock()
			received = append(received, q.Header.Clone())
			mu.Unlock()
		}
	}))
	defer collector.Close()
	c, err := agentrail.New(context.Background(), options(collector.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	names := []string{"traceparent", "tracestate", "baggage", "sentry-trace", "x-agentrail-trace-handoff", "x-agentrail-cli"}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, err := http.NewRequestWithContext(r.Context(), http.MethodGet, collector.URL+"/ingest/otlp/v1/traces", nil)
		if err != nil {
			t.Error(err)
			return
		}
		for _, name := range names {
			q.Header.Set(name, "set-by-the-application")
		}
		resp, err := c.Transport(nil).RoundTrip(q)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://app.test/", nil))
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("collector received %d calls", len(received))
	}
	for _, name := range names {
		if received[0].Get(name) != "" {
			t.Fatalf("collector call kept %s: %v", name, received[0])
		}
	}
	if n := c.Diagnostics().Spans.Accepted; n != 1 {
		t.Fatalf("recorded %d spans; the request's entry alone, not its collector call", n)
	}
}

// This fails if Client.Log drops a log's event name or replaces the times
// the application gave it.
func TestLogEventNameAndOriginalTimes(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	observed := stamp.Add(time.Second)
	c.Log(context.Background(), agentrail.StructuredLog{EventName: "source.event", Timestamp: stamp, ObservedTimestamp: observed})
	c.Log(context.Background(), agentrail.StructuredLog{EventName: "direct.event"})
	c.Flush(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	for _, packet := range r.logs {
		for _, rl := range packet.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, log := range sl.LogRecords {
					seen[log.EventName] = true
					if log.EventName == "source.event" && (log.TimeUnixNano != uint64(stamp.UnixNano()) || log.ObservedTimeUnixNano != uint64(observed.UnixNano())) {
						t.Fatal("changed source timestamps")
					}
				}
			}
		}
	}
	if !seen["source.event"] || !seen["direct.event"] {
		t.Fatalf("lost log event names: %v", seen)
	}
}

func TestRemoteCollectorPortAndPathDoNotExemptApplication(t *testing.T) {
	_, s := collector(t)
	o := options(s.URL)
	o.Collector.Endpoint = "https://collector.example"
	o.OnError = func(context.Context, agentrail.CaptureError) {}
	c, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Shutdown(ctx)
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.Milestone(r.Context(), "forward", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "forward"})
	}))
	q := httptest.NewRequest("GET", "https://app.example/v1/logs", nil)
	q = q.WithContext(context.WithValue(q.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 443}))
	h.ServeHTTP(httptest.NewRecorder(), q)
	if c.Diagnostics().Spans.Accepted != 2 {
		t.Fatal("remote collector's shared port/path suppressed application entry")
	}
	cancel()
}
