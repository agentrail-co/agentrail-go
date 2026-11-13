package agentrail_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

type receiver struct {
	mu     sync.Mutex
	traces []*collectortrace.ExportTraceServiceRequest
	logs   []*collectorlog.ExportLogsServiceRequest
}

func collector(t *testing.T) (*receiver, *httptest.Server) {
	t.Helper()
	r := &receiver{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "Bearer synthetic-source" {
			t.Error("missing source auth")
		}
		if q.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("not protobuf")
		}
		b, err := io.ReadAll(q.Body)
		if err != nil {
			t.Error(err)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		switch q.URL.Path {
		case "/ingest/otlp/v1/traces":
			v := new(collectortrace.ExportTraceServiceRequest)
			if err := proto.Unmarshal(b, v); err != nil {
				t.Error(err)
			}
			r.traces = append(r.traces, v)
		case "/ingest/otlp/v1/logs":
			v := new(collectorlog.ExportLogsServiceRequest)
			if err := proto.Unmarshal(b, v); err != nil {
				t.Error(err)
			}
			r.logs = append(r.logs, v)
		default:
			t.Errorf("unexpected path %s", q.URL.Path)
		}
	}))
	t.Cleanup(s.Close)
	return r, s
}
func options(endpoint string) agentrail.Options {
	return agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: endpoint + "/ingest/otlp", Secret: "synthetic-source"}, HashKey: "synthetic-workspace-key", ServiceName: "test-api"}
}
func TestHTTPIdentityMilestoneAndLogsExportSanitized(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if err := c.Identify(q.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalAgentRegistration, ID: "private-principal"}); err != nil {
			t.Error(err)
		}
		ctx, end := c.StartMilestone(q.Context(), "checkout.confirm", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "checkout.confirm"})
		defer end(nil)
		c.Log(ctx, agentrail.StructuredLog{SeverityNumber: 17, ErrorType: "Declined", EventName: "checkout.failed", Fields: map[string]any{"message": "private-message"}})
		w.WriteHeader(http.StatusCreated)
	}))
	app := httptest.NewServer(h)
	defer app.Close()
	resp, err := http.Get(app.URL + "/checkout?secret=private-query")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatal(resp.StatusCode)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.traces) == 0 || len(r.logs) == 0 {
		t.Fatalf("missing signals: traces=%d logs=%d", len(r.traces), len(r.logs))
	}
	all := []proto.Message{}
	for _, v := range r.traces {
		all = append(all, v)
	}
	for _, v := range r.logs {
		all = append(all, v)
	}
	for _, m := range all {
		raw := fmt.Sprint(m)
		for _, secret := range []string{"private-principal", "private-message", "private-query"} {
			if strings.Contains(raw, secret) {
				t.Fatalf("export leaked %s", secret)
			}
		}
	}
	d := c.Diagnostics()
	if d.Spans.Exported != 2 || d.Logs.Exported != 1 || d.Spans.Pending != 0 {
		t.Fatalf("wrong diagnostics: %+v", d)
	}
}
func contains(s, v string) bool {
	for i := 0; i+len(v) <= len(s); i++ {
		if s[i:i+len(v)] == v {
			return true
		}
	}
	return false
}
func TestInvalidConfiguration(t *testing.T) {
	for _, change := range []func(*agentrail.Options){func(o *agentrail.Options) { o.HashKey = "" }, func(o *agentrail.Options) { o.Collector.Endpoint = "https://example.com/?secret=x" }, func(o *agentrail.Options) { o.OutboundOrigins = []string{"https://example.com/path"} }, func(o *agentrail.Options) {
		o.Queue.ExportTimeout = time.Second
		o.Queue.FlushTimeout = time.Millisecond
	}, func(o *agentrail.Options) { o.MCP.CapturePayloads = true }} {
		o := options("http://localhost")
		change(&o)
		if c, err := agentrail.New(context.Background(), o); err == nil {
			c.Shutdown(context.Background())
			t.Fatal("accepted invalid configuration")
		}
	}
}
