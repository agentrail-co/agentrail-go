package coexistence_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	sentryhttpclient "github.com/getsentry/sentry-go/httpclient"
)

// The caller's Sentry trace.
const (
	sentryTrace = "771a43a4192642f0b136d5159a501700"
	sentrySpan  = "a5c3e0f7b2d14e61"
)

// This fails if Agentrail's capture stops Sentry's trace continuing: its
// transaction no longer continuing the caller's, its sentry-trace, baggage or
// traceparent taken off the application's calls, or the allowed origin's
// service no longer continuing it. It fails too if our own spans stop
// arriving, or change, beside Sentry.
func TestRunsBesideSentry(t *testing.T) {
	events := &sentry.MockTransport{}
	if err := sentry.Init(sentry.ClientOptions{Dsn: "https://public@sentry.example.com/1", EnableTracing: true, TracesSampleRate: 1, PropagateTraceparent: true, Transport: events}); err != nil {
		t.Fatal(err)
	}
	defer sentry.Init(sentry.ClientOptions{})
	incoming := http.Header{
		"Sentry-Trace": {sentryTrace + "-" + sentrySpan + "-1"},
		"Baggage":      {"sentry-trace_id=" + sentryTrace + ",sentry-public_key=public,sentry-sample_rate=1,sentry-sampled=true"},
	}
	got := serve(t, incoming, &tracer{
		handler: sentryhttp.New(sentryhttp.Options{}).Handle,
		transport: func(rt http.RoundTripper) http.RoundTripper {
			return sentryhttpclient.NewSentryRoundTripper(rt)
		},
		downstream: sentryhttp.New(sentryhttp.Options{}).Handle,
	})
	sentry.Flush(0)

	// (1) Sentry continued the caller's trace through our capture, and the
	// allowed origin's service, given our traceparent, continued Sentry's.
	transactions := map[string]*sentry.Event{}
	for _, e := range events.Events() {
		if e.Type == "transaction" {
			transactions[e.Transaction] = e
		}
	}
	for _, name := range []string{"GET /orders", "GET /allowed"} {
		transaction := transactions[name]
		if transaction == nil {
			t.Fatalf("Sentry sent no %s transaction: %v", name, transactions)
		}
		trace := transaction.Contexts["trace"]
		if id, _ := trace["trace_id"].(sentry.TraceID); id.String() != sentryTrace {
			t.Fatalf("Sentry's %s transaction did not continue the caller's trace: %v", name, trace)
		}
	}
	if n := len(transactions["GET /orders"].Spans); n != 2 {
		t.Fatalf("Sentry recorded %d calls", n)
	}
	// (2) The ordinary origin got Sentry's headers, its own call's span named.
	header := got.ordinary.Get("sentry-trace")
	if !strings.HasPrefix(header, sentryTrace+"-") || strings.Contains(header, sentrySpan) {
		t.Fatalf("ordinary origin got sentry-trace %q", header)
	}
	if !strings.Contains(got.ordinary.Get("baggage"), "sentry-trace_id="+sentryTrace) || !strings.HasPrefix(got.ordinary.Get("traceparent"), "00-"+sentryTrace+"-") {
		t.Fatalf("ordinary origin lost Sentry's headers: %v", got.ordinary)
	}
	// The allowed origin keeps Sentry's own header beside our traceparent.
	if !strings.HasPrefix(got.allowed.Get("sentry-trace"), sentryTrace+"-") {
		t.Fatalf("allowed origin lost sentry-trace: %v", got.allowed)
	}
	// (3) Our spans and headers are as without Sentry.
	checkOurs(t, got, serve(t, incoming, nil))
}
