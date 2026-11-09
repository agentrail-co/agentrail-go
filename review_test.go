package agentrail_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	"go.opentelemetry.io/otel/attribute"
	apilog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestCollectorPrefixDoesNotBypassPropagationBoundary(t *testing.T) {
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
		q, _ := http.NewRequestWithContext(r.Context(), "GET", "http://collector.example.attacker.test/", nil)
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
	if received.Get("baggage") != "" || received.Get("traceparent") != "" {
		t.Fatal("collector hostname prefix bypassed propagation cleaning")
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
func TestOTelLogEventNameAndOriginalTimes(t *testing.T) {
	r, s := collector(t)
	p, err := agentrail.NewProcessors(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p.SpanProcessor))
	defer tp.Shutdown(context.Background())
	c, err := p.Bind(tp)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(p.LogProcessor))
	defer lp.Shutdown(context.Background())
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	observed := stamp.Add(time.Second)
	var rec apilog.Record
	rec.SetTimestamp(stamp)
	rec.SetObservedTimestamp(observed)
	rec.SetEventName("source.event")
	rec.AddAttributes(attribute.String("private.field", "private-value"))
	lp.Logger("source").Emit(context.Background(), rec)
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
