package mcp_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	agentrailmcp "github.com/agentrail-co/agentrail-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type capture struct {
	mu    sync.Mutex
	spans []*tracepb.Span
}

func newCapture(t *testing.T) (*agentrail.Client, *capture) {
	t.Helper()
	r := new(capture)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !strings.HasSuffix(req.URL.Path, "/traces") {
			return
		}
		message := new(collectortrace.ExportTraceServiceRequest)
		if err := proto.Unmarshal(raw, message); err != nil {
			t.Error(err)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, resource := range message.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				r.spans = append(r.spans, scope.Spans...)
			}
		}
	}))
	t.Cleanup(server.Close)
	c, err := agentrail.New(context.Background(), agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: server.URL, Secret: "synthetic-source"}, HashKey: "synthetic-key", ServiceName: "mcp-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return c, r
}
func spans(t *testing.T, c *agentrail.Client, r *capture) []*tracepb.Span {
	t.Helper()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*tracepb.Span(nil), r.spans...)
}
func attr(s *tracepb.Span, key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value.GetStringValue()
		}
	}
	return ""
}

type input struct {
	Secret string `json:"secret"`
}
type output struct {
	Value string `json:"value"`
}

// This fails if an adapter ignores the registered name, uses raw resource URIs,
// loses metadata from either protocol generation, or fails to record IsError.
func TestOfficialServerOperationsOverTransports(t *testing.T) {
	for _, tc := range []struct{ transport, version string }{{"memory", "2026-07-28"}, {"memory", "2025-11-25"}, {"http", "2026-07-28"}, {"http", "2025-11-25"}, {"sse", "2024-11-05"}} {
		t.Run(tc.transport+"/"+tc.version, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, receiver := newCapture(t)
			server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
			sdk.AddTool(server, &sdk.Tool{Name: "lookup"}, agentrailmcp.Tool(c, "registered.lookup", func(ctx context.Context, req *sdk.CallToolRequest, in input) (*sdk.CallToolResult, output, error) {
				if in.Secret != "private-argument" {
					t.Errorf("argument changed: %q", in.Secret)
				}
				return nil, output{Value: "private-result"}, nil
			}))
			sdk.AddTool(server, &sdk.Tool{Name: "failure"}, agentrailmcp.Tool(c, "registered.failure", func(ctx context.Context, req *sdk.CallToolRequest, in input) (*sdk.CallToolResult, output, error) {
				return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "private-tool-error"}}}, output{}, nil
			}))
			server.AddTool(&sdk.Tool{Name: "lowlevel", InputSchema: map[string]any{"type": "object"}}, agentrailmcp.ToolHandler(c, "registered.lowlevel", func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return nil, errors.New("private-protocol-error")
			}))
			server.AddResource(&sdk.Resource{Name: "private-resource-name", URI: "test://private-resource-uri"}, agentrailmcp.Resource(c, "registered.resource", func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
				return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, Text: "private-resource-content"}}}, nil
			}))
			server.AddPrompt(&sdk.Prompt{Name: "prompt"}, agentrailmcp.Prompt(c, "registered.prompt", func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
				return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: "private-prompt-content"}}}}, nil
			}))
			var transport sdk.Transport
			if tc.transport == "memory" {
				st, ct := sdk.NewInMemoryTransports()
				session, err := server.Connect(ctx, st, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer session.Close()
				transport = ct
			} else {
				var handler http.Handler
				if tc.transport == "http" {
					handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: tc.version >= "2026-07-28"})
				} else {
					handler = sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return server }, nil)
				}
				app := httptest.NewServer(handler)
				defer app.Close()
				if tc.transport == "http" {
					transport = &sdk.StreamableClientTransport{Endpoint: app.URL, DisableStandaloneSSE: true}
				} else {
					transport = &sdk.SSEClientTransport{Endpoint: app.URL}
				}
			}
			client := sdk.NewClient(&sdk.Implementation{Name: "synthetic-caller", Version: "2.3"}, nil)
			session, err := client.Connect(ctx, transport, &sdk.ClientSessionOptions{ProtocolVersion: tc.version})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "lookup", Arguments: map[string]any{"secret": "private-argument"}})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError || !strings.Contains(result.Content[0].(*sdk.TextContent).Text, "private-result") {
				t.Fatalf("tool result changed: %+v", result)
			}
			result, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "failure", Arguments: map[string]any{"secret": "private-argument"}})
			if err != nil || !result.IsError {
				t.Fatalf("IsError changed: %+v, %v", result, err)
			}
			if _, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "lowlevel", Arguments: map[string]any{}}); err == nil {
				t.Fatal("protocol error swallowed")
			}
			resource, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "test://private-resource-uri"})
			if err != nil || resource.Contents[0].Text != "private-resource-content" {
				t.Fatalf("resource result changed: %+v, %v", resource, err)
			}
			prompt, err := session.GetPrompt(ctx, &sdk.GetPromptParams{Name: "prompt"})
			if err != nil || prompt.Messages[0].Content.(*sdk.TextContent).Text != "private-prompt-content" {
				t.Fatalf("prompt result changed: %+v, %v", prompt, err)
			}
			got := spans(t, c, receiver)
			if len(got) != 5 {
				t.Fatalf("got %d operation spans", len(got))
			}
			expected := map[string]struct{ method, kind string }{
				"mcp.tools/call:registered.lookup":       {"tools/call", "tool.call"},
				"mcp.tools/call:registered.failure":      {"tools/call", "tool.call"},
				"mcp.tools/call:registered.lowlevel":     {"tools/call", "tool.call"},
				"mcp.resources/read:registered.resource": {"resources/read", "mcp.resources/read"},
				"mcp.prompts/get:registered.prompt":      {"prompts/get", "mcp.prompts/get"},
			}
			for _, span := range got {
				operation, ok := expected[span.Name]
				if !ok {
					t.Fatalf("unregistered span name %q", span.Name)
				}
				delete(expected, span.Name)
				if attr(span, "rpc.method") != operation.method || attr(span, "agentrail.step.kind") != operation.kind || attr(span, "agentrail.step.surface") != "mcp" || attr(span, "agentrail.mcp.client.name") != "synthetic-caller" || attr(span, "agentrail.mcp.client.version") != "2.3" || attr(span, "agentrail.mcp.protocol.version") != tc.version {
					t.Fatalf("missing MCP metadata: %v", span)
				}
				failed := span.Name == "mcp.tools/call:registered.failure" || span.Name == "mcp.tools/call:registered.lowlevel"
				if (span.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR) != failed {
					t.Fatalf("wrong operation status: %v", span)
				}
				// This fails if the OpenTelemetry MCP names are dropped, a
				// resource is named by its URI, or a failed tool result reads
				// like a handler that returned an error.
				registered := span.Name[strings.Index(span.Name, ":")+1:]
				wantTool, wantPrompt := "", ""
				switch operation.method {
				case "tools/call":
					wantTool = registered
				case "prompts/get":
					wantPrompt = registered
				}
				if attr(span, "mcp.method.name") != operation.method || attr(span, "gen_ai.tool.name") != wantTool || attr(span, "gen_ai.prompt.name") != wantPrompt || attr(span, "mcp.resource.uri") != "" {
					t.Fatalf("wrong OpenTelemetry MCP names: %v", span)
				}
				if (attr(span, "error.type") == "tool_error") != (span.Name == "mcp.tools/call:registered.failure") {
					t.Fatalf("wrong error type: %v", span)
				}
				if len(span.ParentSpanId) != 0 {
					t.Fatalf("standalone operation unexpectedly parented: %v", span)
				}
				for _, a := range span.Attributes {
					if strings.HasPrefix(a.Key, "agentrail.principal") {
						t.Fatalf("untrusted client became identity: %v", a)
					}
				}
				if strings.Contains(span.String(), "private-") {
					t.Fatalf("leaked protocol contents: %v", span)
				}
			}
		})
	}
}

// This fails if wrapper return values are reconstructed or if panic capture
// swallows or changes the exact application panic.
func TestWrappersPreserveErrorsResultsAndPanics(t *testing.T) {
	c, receiver := newCapture(t)
	ctx := context.Background()
	original := errors.New("private-original-error")
	toolResult := &sdk.CallToolResult{IsError: true}
	tool := agentrailmcp.Tool(c, "tool.error", func(context.Context, *sdk.CallToolRequest, input) (*sdk.CallToolResult, output, error) {
		return toolResult, output{Value: "unchanged"}, original
	})
	result, out, err := tool(ctx, &sdk.CallToolRequest{}, input{})
	if result != toolResult || out.Value != "unchanged" || err != original {
		t.Fatal("typed tool changed returns")
	}
	resourceResult := &sdk.ReadResourceResult{}
	resource := agentrailmcp.Resource(c, "resource.error", func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return resourceResult, original
	})
	rr, err := resource(ctx, &sdk.ReadResourceRequest{})
	if rr != resourceResult || err != original {
		t.Fatal("resource changed returns")
	}
	promptResult := &sdk.GetPromptResult{}
	prompt := agentrailmcp.Prompt(c, "prompt.error", func(context.Context, *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		return promptResult, original
	})
	pr, err := prompt(ctx, &sdk.GetPromptRequest{})
	if pr != promptResult || err != original {
		t.Fatal("prompt changed returns")
	}
	marker := &struct{ Value string }{"private-panic"}
	panicTool := agentrailmcp.Tool(c, "tool.panic", func(context.Context, *sdk.CallToolRequest, input) (*sdk.CallToolResult, output, error) { panic(marker) })
	func() {
		defer func() {
			if p := recover(); p != marker {
				t.Errorf("panic changed: %v", p)
			}
		}()
		panicTool(ctx, &sdk.CallToolRequest{}, input{})
		t.Error("panic swallowed")
	}()
	got := spans(t, c, receiver)
	if len(got) != 4 {
		t.Fatalf("got %d spans", len(got))
	}
	for _, span := range got {
		if span.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR {
			t.Fatalf("failed operation successful: %v", span)
		}
		if strings.Contains(span.String(), "private-") {
			t.Fatalf("leaked error/panic: %v", span)
		}
	}
}

// This fails if a wrapper starts from a background context, dropping the
// caller's active milestone, cancellation, or application context values.
func TestPreservesCallerContextAndManagedParent(t *testing.T) {
	c, receiver := newCapture(t)
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "application-value"))
	ctx, end := c.StartMilestone(ctx, "caller", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "caller"})
	cancel()
	wrapper := agentrailmcp.Prompt(c, "prompt.child", func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		if ctx.Value(contextKey{}) != "application-value" || ctx.Err() != context.Canceled {
			t.Error("caller context lost")
		}
		return &sdk.GetPromptResult{}, nil
	})
	if _, err := wrapper(ctx, &sdk.GetPromptRequest{}); err != nil {
		t.Fatal(err)
	}
	end(nil)
	got := spans(t, c, receiver)
	if len(got) != 2 {
		t.Fatalf("got %d spans", len(got))
	}
	var parent, child *tracepb.Span
	for _, span := range got {
		if span.Name == "caller" {
			parent = span
		}
		if span.Name == "mcp.prompts/get:prompt.child" {
			child = span
		}
	}
	if parent == nil || child == nil || string(parent.TraceId) != string(child.TraceId) || string(child.ParentSpanId) != string(parent.SpanId) {
		t.Fatalf("lost managed parent: %v", got)
	}
}

// This fails if a registered label exceeding the backend's 256 UTF-16-unit
// limit silently prevents capture, or truncation cuts a supplementary rune.
func TestLongRegisteredNameIsBoundedForCapture(t *testing.T) {
	c, receiver := newCapture(t)
	wrapper := agentrailmcp.Resource(c, strings.Repeat("😀", 200), func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{}, nil
	})
	if _, err := wrapper(context.Background(), &sdk.ReadResourceRequest{}); err != nil {
		t.Fatal(err)
	}
	got := spans(t, c, receiver)
	if len(got) != 1 {
		t.Fatalf("got %d spans for long registration name", len(got))
	}
	prefix := "mcp.resources/read:"
	expected := prefix + strings.Repeat("😀", (256-len(prefix))/2)
	if got[0].Name != expected {
		t.Fatalf("name not safely bounded: %q", got[0].Name)
	}
}
