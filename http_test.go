package agentrail_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
)

// This fails if an allowed origin's call stops being signed or keeps the
// application's tracestate beside our traceparent, if our signed context
// follows a redirect to an origin not allowed, or if a public traceparent is
// trusted as a parent.
func TestSignedServicesAndRedirectBoundary(t *testing.T) {
	r, collector := collector(t)
	o := options(collector.URL)
	downstream, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer downstream.Shutdown(context.Background())
	var targetHeaders http.Header
	target := httptest.NewServer(downstream.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		targetHeaders = q.Header.Clone()
		io.WriteString(w, "okay")
	})))
	defer target.Close()
	var foreignHeaders http.Header
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { foreignHeaders = q.Header.Clone() }))
	defer foreign.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { http.Redirect(w, q, foreign.URL, http.StatusFound) }))
	defer redirect.Close()
	o.OutboundOrigins = []string{target.URL, redirect.URL}
	upstream, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Shutdown(context.Background())
	network := &http.Client{Transport: upstream.Transport(nil)}
	h := upstream.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		for _, url := range []string{target.URL + "/x%2Fy?x=1+2&x=%20", redirect.URL} {
			req, _ := http.NewRequestWithContext(q.Context(), "GET", url, nil)
			req.Host = "untrusted-host.example"
			req.Header.Set("baggage", "private-baggage")
			req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
			req.Header.Set("sentry-trace", "private-sentry")
			req.Header.Set("tracestate", "private-state")
			resp, err := network.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}))
	req := httptest.NewRequest("GET", "http://app.test/", nil)
	req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if targetHeaders.Get("x-agentrail-trace-handoff") == "" || targetHeaders.Get("tracestate") != "" {
		t.Fatalf("allowed origin got %v", targetHeaders)
	}
	// The application's other headers are its own and go on as it set them.
	if targetHeaders.Get("baggage") != "private-baggage" || targetHeaders.Get("sentry-trace") != "private-sentry" {
		t.Fatalf("allowed origin lost the application's headers: %v", targetHeaders)
	}
	if foreignHeaders.Get("x-agentrail-trace-handoff") != "" || foreignHeaders.Get("traceparent") != "00-11111111111111111111111111111111-2222222222222222-01" {
		t.Fatalf("redirect carried our context: %v", foreignHeaders)
	}
	upstream.Flush(context.Background())
	downstream.Flush(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	ss := spans(r)
	verified := 0
	roots := 0
	for _, s := range ss {
		if s.Kind == 2 {
			if attr(s.Attributes, "agentrail.ingress.verified").GetBoolValue() {
				verified++
			} else {
				roots++
				if len(s.ParentSpanId) != 0 {
					t.Error("trusted public trace parent")
				}
				if len(s.Links) != 1 {
					t.Error("missing directional link")
				}
			}
		}
	}
	if verified != 1 || roots != 1 {
		t.Fatalf("wrong entry verification %d/%d", verified, roots)
	}
}
func TestMiddlewarePreservesStreamStatusAndBody(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("lost flusher")
		}
		body, _ := io.ReadAll(q.Body)
		if string(body) != "untouched" {
			t.Error("read request body")
		}
		w.WriteHeader(201)
		io.WriteString(w, "stream")
		w.(http.Flusher).Flush()
		w.WriteHeader(503)
	}))
	out := httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest("POST", "http://app.test/", strings.NewReader("untouched")))
	if out.Code != 201 || out.Body.String() != "stream" || !out.Flushed {
		t.Fatal("changed response")
	}
	c.Flush(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sp := range spans(r) {
		if got := attr(sp.Attributes, "http.response.status_code").GetIntValue(); got != 201 {
			t.Fatalf("captured status %d instead of first status", got)
		}
	}
}
func TestDiagnosticLoggerSuppressionAndCallbackPanic(t *testing.T) {
	_, s := collector(t)
	var c *agentrail.Client
	o := options(s.URL)
	calls := 0
	o.OnError = func(ctx context.Context, e agentrail.CaptureError) {
		calls++
		c.Log(ctx, agentrail.StructuredLog{EventName: "diagnostic"})
		panic("private-callback-panic")
	}
	var err error
	c, err = agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	if err := c.Identify(context.Background(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: "synthetic-user"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || c.Diagnostics().Logs.Accepted != 0 {
		t.Fatal("recursive diagnostics")
	}
}
func TestShutdownHonorsConcurrentCallerDeadline(t *testing.T) {
	_, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
