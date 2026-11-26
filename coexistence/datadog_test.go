package coexistence_test

import (
	"net/http"
	"strconv"
	"testing"

	httptrace "github.com/DataDog/dd-trace-go/contrib/net/http/v2"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/mocktracer"
)

// The caller's Datadog trace.
const (
	datadogTrace  = "6312417489302815337"
	datadogParent = "4201538924611092263"
)

// This fails if Agentrail's capture stops Datadog's trace continuing: its
// request span no longer continuing the caller's, or its x-datadog-* or
// traceparent headers taken off the application's calls. It fails too if
// our own spans stop arriving, or change, beside Datadog.
func TestRunsBesideDatadog(t *testing.T) {
	mt := mocktracer.Start()
	defer mt.Stop()
	incoming := http.Header{
		"X-Datadog-Trace-Id":          {datadogTrace},
		"X-Datadog-Parent-Id":         {datadogParent},
		"X-Datadog-Sampling-Priority": {"1"},
	}
	got := serve(t, incoming, func(h http.Handler) http.Handler {
		return httptrace.WrapHandler(h, "orders-api", "GET /orders")
	}, func(rt http.RoundTripper) http.RoundTripper { return httptrace.WrapRoundTripper(rt) })

	// (1) Datadog continued the caller's trace through our capture.
	var server *mocktracer.Span
	calls := map[uint64]*mocktracer.Span{}
	for _, s := range mt.FinishedSpans() {
		switch s.Tag(ext.SpanKind) {
		case ext.SpanKindServer:
			server = s
		case ext.SpanKindClient:
			calls[s.SpanID()] = s
		}
	}
	if server == nil || strconv.FormatUint(server.Context().TraceIDLower(), 10) != datadogTrace || strconv.FormatUint(server.ParentID(), 10) != datadogParent {
		t.Fatalf("Datadog's request span did not continue the caller's trace: %v", server)
	}
	if len(calls) != 2 {
		t.Fatalf("Datadog recorded %d calls", len(calls))
	}
	for _, s := range calls {
		if s.ParentID() != server.SpanID() {
			t.Fatalf("Datadog's call is not a child of its request span: %v", s)
		}
	}
	// (2) The ordinary origin got Datadog's headers, its own call's span named.
	parent, err := strconv.ParseUint(got.ordinary.Get("x-datadog-parent-id"), 10, 64)
	if _, ok := calls[parent]; got.ordinary.Get("x-datadog-trace-id") != datadogTrace || err != nil || !ok {
		t.Fatalf("ordinary origin got %v", got.ordinary)
	}
	if got.ordinary.Get("traceparent") == "" || got.ordinary.Get("tracestate") == "" {
		t.Fatalf("ordinary origin lost Datadog's W3C headers: %v", got.ordinary)
	}
	// The allowed origin keeps Datadog's own headers beside our traceparent.
	if got.allowed.Get("x-datadog-trace-id") != datadogTrace {
		t.Fatalf("allowed origin lost x-datadog-trace-id: %v", got.allowed)
	}
	// (3) Our spans and headers are as without Datadog.
	checkOurs(t, got, serve(t, incoming, unchanged[http.Handler], unchanged[http.RoundTripper]))
}
