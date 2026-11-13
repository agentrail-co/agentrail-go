package agentrail

import (
	"context"
	"encoding/json"
	"testing"

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
