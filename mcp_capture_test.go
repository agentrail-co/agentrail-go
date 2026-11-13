package agentrail_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/mcpspan"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// This fails if payload text held by the span queue, the in-flight batch
// included, can grow past its budget, if the span is dropped with its payload,
// or if released exports never return their budget.
func TestPayloadBudgetDropsPayloadsNotSpans(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	got := []*tracepb.Span{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/traces") {
			return
		}
		<-release
		v := new(collectortrace.ExportTraceServiceRequest)
		if err := proto.Unmarshal(raw, v); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, rs := range v.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				got = append(got, ss.Spans...)
			}
		}
	}))
	defer server.Close()
	var codes []string
	o := options(server.URL)
	o.MCP.CapturePayloads = true
	o.Queue = agentrail.QueueOptions{BatchSize: 1, ScheduledDelay: time.Millisecond, ExportTimeout: 30 * time.Second, FlushTimeout: 30 * time.Second, ShutdownTimeout: 30 * time.Second}
	o.OnError = func(_ context.Context, e agentrail.CaptureError) {
		mu.Lock()
		codes = append(codes, e.Code)
		mu.Unlock()
	}
	c, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	// Five 14 MiB results exceed the 64 MiB budget by one while exports wait.
	result, err := json.Marshal(map[string]string{"text": strings.Repeat("a", 14<<20)})
	if err != nil {
		t.Fatal(err)
	}
	call := func(target string) {
		span := mcpspan.Transport(c, context.Background(), mcpspan.Call{Method: "tools/call", Target: target, RequestBytes: 10, Params: json.RawMessage(`{}`)})
		span.End(mcpspan.Outcome{Responded: true, ResponseBytes: len(result), Result: result})
	}
	for _, target := range []string{"one", "two", "three", "four", "five"} {
		call(target)
	}
	close(release)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	call("after")
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(codes, ",") != "payload_budget_exceeded" {
		t.Fatalf("diagnostics %v", codes)
	}
	carried := map[string]bool{}
	for _, s := range got {
		for _, a := range s.Attributes {
			if a.Key == "agentrail.mcp.response.payload" && bytes.Equal([]byte(a.Value.GetStringValue()), result) {
				carried[s.Name] = true
			}
		}
	}
	if len(got) != 6 || len(carried) != 5 || carried["mcp.tools/call:five"] || !carried["mcp.tools/call:after"] {
		t.Fatalf("%d spans; payloads on %v", len(got), carried)
	}
}
