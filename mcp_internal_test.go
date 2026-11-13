package agentrail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unsafe"

	"github.com/agentrail-co/agentrail-go/internal/mcpspan"
)

// This fails if a pending transport span keeps the request's params, which
// can be megabytes, when payload capture is off and nothing will send them.
func TestPendingCallsKeepParamsOnlyForCapture(t *testing.T) {
	for _, capture := range []bool{false, true} {
		o := Options{Collector: CollectorOptions{Endpoint: "http://127.0.0.1:1", Secret: "synthetic-source"}, HashKey: "synthetic-key", ServiceName: "mcp-test"}
		if capture {
			o.MCP = MCPOptions{CapturePayloads: true, Redact: func(p MCPPayload) (MCPPayload, bool) { return p, true }}
		}
		c, err := New(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		span, ok := c.mcpTransport(context.Background(), mcpspan.Call{Method: "tools/call", Target: "x", Params: json.RawMessage(`{"name":"x"}`)}).(*transportSpan)
		if !ok {
			t.Fatal("no transport span")
		}
		if kept := span.call.Params != nil; kept != capture {
			t.Fatalf("capture %v: params kept %v", capture, kept)
		}
		span.End(mcpspan.Outcome{})
		c.Shutdown(context.Background())
	}
}

// This fails if a cut payload shares the original's memory: the budget
// counts only the cut length, so queued spans would pin whole results.
func TestCutPayloadsDoNotPinTheirOriginal(t *testing.T) {
	request, response := `{"a":1}`, strings.Repeat("é", 1<<20)
	for _, limit := range []int{1 << 20, 1 << 10} {
		q, _, r, rc := fitPayload(request, response, limit)
		if !rc || len(q)+len(r) > limit {
			t.Fatalf("limit %d: not cut", limit)
		}
		if unsafe.StringData(r) == unsafe.StringData(response) {
			t.Fatalf("limit %d: the cut response shares the original's memory", limit)
		}
	}
	long := strings.Repeat("a", 1<<20)
	if q, qc, r, rc := fitPayload(long, long, 1<<10); !qc || !rc || unsafe.StringData(q) == unsafe.StringData(long) || unsafe.StringData(r) == unsafe.StringData(long) {
		t.Fatal("a cut payload shares the original's memory")
	}
}
