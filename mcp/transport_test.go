package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/handoff"
	agentrailmcp "github.com/agentrail-co/agentrail-go/mcp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// sdkContract is the part of the Argus contract these tests assert against.
type sdkContract struct {
	OTLP struct {
		SpanAttributes map[string]string `json:"spanAttributes"`
		MCPAttributes  map[string]string `json:"mcpAttributes"`
		Limits         struct {
			BodyBytes        int `json:"bodyBytes"`
			MCPPayloadBytes  int `json:"mcpPayloadBytes"`
			ExportBatchBytes int `json:"exportBatchBytes"`
		} `json:"limits"`
		Fixtures struct {
			Accepted []struct {
				Name    string `json:"name"`
				Request struct {
					ResourceSpans []struct {
						ScopeSpans []struct {
							Spans []struct {
								Name       string `json:"name"`
								Attributes []struct {
									Key string `json:"key"`
								} `json:"attributes"`
							} `json:"spans"`
						} `json:"scopeSpans"`
					} `json:"resourceSpans"`
				} `json:"request"`
			} `json:"accepted"`
		} `json:"fixtures"`
	} `json:"otlp"`
}

var loadContract = sync.OnceValues(func() (*sdkContract, error) {
	raw, err := os.ReadFile("../testdata/sdk-contract.json")
	if err != nil {
		return nil, err
	}
	c := new(sdkContract)
	return c, json.Unmarshal(raw, c)
})

func contract(t *testing.T) *sdkContract {
	t.Helper()
	c, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// key is the attribute the contract names for an MCP fact.
func key(t *testing.T, name string) string {
	t.Helper()
	k, ok := contract(t).OTLP.MCPAttributes[name]
	if !ok {
		t.Fatalf("contract names no MCP attribute %q", name)
	}
	return k
}

// named is every attribute key the contract names for an SDK MCP span.
func named(t *testing.T) map[string]bool {
	t.Helper()
	c := contract(t)
	out := map[string]bool{}
	for _, k := range c.OTLP.SpanAttributes {
		out[k] = true
	}
	for _, k := range c.OTLP.MCPAttributes {
		out[k] = true
	}
	for _, f := range c.OTLP.Fixtures.Accepted {
		if !strings.Contains(f.Name, "mcp") {
			continue
		}
		for _, r := range f.Request.ResourceSpans {
			for _, s := range r.ScopeSpans {
				for _, span := range s.Spans {
					for _, a := range span.Attributes {
						out[a.Key] = true
					}
				}
			}
		}
	}
	return out
}

func value(s *tracepb.Span, key string) (*commonpb.AnyValue, bool) {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}
func has(s *tracepb.Span, key string) bool { _, ok := value(s, key); return ok }
func intValue(s *tracepb.Span, key string) (int64, bool) {
	v, ok := value(s, key)
	if !ok {
		return 0, false
	}
	_, isInt := v.Value.(*commonpb.AnyValue_IntValue)
	return v.GetIntValue(), isInt
}
func boolValue(s *tracepb.Span, key string) bool {
	v, ok := value(s, key)
	return ok && v.GetBoolValue()
}

// serve connects server through a wrapper and returns the client's transport.
// mode is memory (Transport), http (stateful Streamable HTTP), stateless or
// sse (both through Handler).
func serve(t *testing.T, ctx context.Context, c *agentrail.Client, server *sdk.Server, mode string) sdk.Transport {
	t.Helper()
	if mode == "memory" {
		st, ct := sdk.NewInMemoryTransports()
		session, err := server.Connect(ctx, agentrailmcp.Transport(c, st), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		return ct
	}
	var handler http.Handler
	if mode == "sse" {
		handler = sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return server }, nil)
	} else {
		handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: mode == "stateless"})
	}
	app := httptest.NewServer(c.Middleware(agentrailmcp.Handler(c, handler)))
	t.Cleanup(app.Close)
	if mode == "sse" {
		return &sdk.SSEClientTransport{Endpoint: app.URL}
	}
	return &sdk.StreamableClientTransport{Endpoint: app.URL, DisableStandaloneSSE: true}
}

var transportCases = []struct{ mode, version string }{{"memory", "2025-11-25"}, {"memory", "2026-07-28"}, {"http", "2025-11-25"}, {"stateless", "2026-07-28"}, {"sse", "2024-11-05"}}

func byLayer(t *testing.T, got []*tracepb.Span, layer string) []*tracepb.Span {
	out := []*tracepb.Span{}
	for _, s := range got {
		if attr(s, key(t, "layer")) == layer {
			out = append(out, s)
		}
	}
	return out
}
func named1(t *testing.T, got []*tracepb.Span, name string) *tracepb.Span {
	t.Helper()
	var found *tracepb.Span
	for _, s := range got {
		if s.Name == name {
			if found != nil {
				t.Fatalf("two spans named %q", name)
			}
			found = s
		}
	}
	if found == nil {
		t.Fatalf("no span named %q in %d spans", name, len(got))
	}
	return found
}

// This fails if a transport span is lost or doubled, a handler becomes a
// second step for one tool call, a JSON-RPC error code or IsError result is
// lost, the session hash is dropped under stateful Streamable HTTP, a payload
// is sent with capture off, or any attribute the contract does not name is sent.
func TestTransportSpansOverTransports(t *testing.T) {
	for _, tc := range transportCases {
		t.Run(tc.mode+"/"+tc.version, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, receiver := newCapture(t)
			server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
			sdk.AddTool(server, &sdk.Tool{Name: "lookup"}, agentrailmcp.Tool(c, "registered.lookup", func(ctx context.Context, req *sdk.CallToolRequest, in input) (*sdk.CallToolResult, output, error) {
				return nil, output{Value: "private-result"}, nil
			}))
			sdk.AddTool(server, &sdk.Tool{Name: "failure"}, agentrailmcp.Tool(c, "registered.failure", func(ctx context.Context, req *sdk.CallToolRequest, in input) (*sdk.CallToolResult, output, error) {
				return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "private-tool-error"}}}, output{}, nil
			}))
			server.AddTool(&sdk.Tool{Name: "lowlevel", InputSchema: map[string]any{"type": "object"}}, agentrailmcp.ToolHandler(c, "registered.lowlevel", func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "private-protocol-error"}
			}))
			server.AddResource(&sdk.Resource{Name: "resource", URI: "test://resource"}, agentrailmcp.Resource(c, "registered.resource", func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
				return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, Text: "private-resource-content"}}}, nil
			}))
			server.AddPrompt(&sdk.Prompt{Name: "prompt"}, agentrailmcp.Prompt(c, "registered.prompt", func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
				return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: "private-prompt-content"}}}}, nil
			}))
			transport := serve(t, ctx, c, server, tc.mode)
			client := sdk.NewClient(&sdk.Implementation{Name: "synthetic-caller", Version: "2.3"}, nil)
			session, err := client.Connect(ctx, transport, &sdk.ClientSessionOptions{ProtocolVersion: tc.version})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "lookup", Arguments: map[string]any{"secret": "private-argument"}}); err != nil || r.IsError {
				t.Fatalf("lookup changed: %+v, %v", r, err)
			}
			if r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "failure", Arguments: map[string]any{"secret": "private-argument"}}); err != nil || !r.IsError {
				t.Fatalf("IsError changed: %+v, %v", r, err)
			}
			if _, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "lowlevel", Arguments: map[string]any{}}); err == nil {
				t.Fatal("protocol error swallowed")
			}
			if _, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "test://resource"}); err != nil {
				t.Fatal(err)
			}
			if _, err := session.GetPrompt(ctx, &sdk.GetPromptParams{Name: "prompt"}); err != nil {
				t.Fatal(err)
			}
			got := spans(t, c, receiver)
			transports, handlers := byLayer(t, got, "transport"), byLayer(t, got, "handler")
			steps := 0
			for _, s := range got {
				if attr(s, "agentrail.step.kind") == "tool.call" && boolValue(s, "agentrail.step") {
					steps++
				}
			}
			if steps != 3 {
				t.Fatalf("got %d tool.call steps for 3 tool calls", steps)
			}
			if len(handlers) != 5 {
				t.Fatalf("got %d handler spans", len(handlers))
			}
			allowed := named(t)
			sessionHash := ""
			if tc.mode == "http" {
				if session.ID() == "" {
					t.Fatal("stateful session has no ID")
				}
				sessionHash = handoff.Hash("synthetic-key", session.ID())
			}
			methods := map[string]int{}
			for _, s := range transports {
				method := attr(s, key(t, "method"))
				methods[method]++
				if !boolValue(s, "agentrail.step") || attr(s, "agentrail.step.surface") != "mcp" || s.Name != "mcp."+method && !strings.HasPrefix(s.Name, "mcp."+method+":") {
					t.Fatalf("transport span is not its step: %v", s)
				}
				if strings.HasSuffix(s.Name, ":") {
					t.Fatalf("span named with an empty target: %q", s.Name)
				}
				kind := "mcp." + method
				if method == "tools/call" {
					kind = "tool.call"
				}
				if attr(s, "agentrail.step.kind") != kind {
					t.Fatalf("wrong kind: %v", s)
				}
				if n, ok := intValue(s, key(t, "requestBytes")); !ok || n <= 0 {
					t.Fatalf("no request size: %v", s)
				}
				if attr(s, key(t, "sessionHash")) != sessionHash {
					t.Fatalf("session hash %q, want %q: %v", attr(s, key(t, "sessionHash")), sessionHash, s)
				}
				if strings.HasPrefix(method, "notifications/") == has(s, key(t, "responseBytes")) {
					t.Fatalf("response size on the wrong message: %v", s)
				}
				for _, a := range s.Attributes {
					if !allowed[a.Key] {
						t.Fatalf("attribute %q is not named by the contract", a.Key)
					}
				}
			}
			for _, s := range handlers {
				if has(s, "agentrail.step") {
					t.Fatalf("handler inside a transport span is a step: %v", s)
				}
				for _, a := range s.Attributes {
					if !allowed[a.Key] {
						t.Fatalf("attribute %q is not named by the contract", a.Key)
					}
				}
			}
			if methods["tools/call"] != 3 || methods["resources/read"] != 1 || methods["prompts/get"] != 1 {
				t.Fatalf("transport spans by method: %v", methods)
			}
			if tc.version < "2026-07-28" && (methods["initialize"] != 1 || methods["notifications/initialized"] != 1) {
				t.Fatalf("handshake not captured: %v", methods)
			}
			for _, pair := range [][2]string{{"mcp.tools/call:lookup", "mcp.tools/call:registered.lookup"}, {"mcp.tools/call:failure", "mcp.tools/call:registered.failure"}, {"mcp.tools/call:lowlevel", "mcp.tools/call:registered.lowlevel"}, {"mcp.resources/read:test://resource", "mcp.resources/read:registered.resource"}, {"mcp.prompts/get:prompt", "mcp.prompts/get:registered.prompt"}} {
				parent, child := named1(t, transports, pair[0]), named1(t, handlers, pair[1])
				if !bytes.Equal(child.ParentSpanId, parent.SpanId) || !bytes.Equal(child.TraceId, parent.TraceId) {
					t.Fatalf("%s is not the child of %s", pair[1], pair[0])
				}
				if attr(parent, key(t, "clientName")) != "synthetic-caller" || attr(parent, key(t, "clientVersion")) != "2.3" || attr(parent, key(t, "protocolVersion")) != tc.version {
					t.Fatalf("client metadata lost: %v", parent)
				}
				// Fails if either layer loses standard MCP names, or a resource URI is exported as one.
				for _, s := range []*tracepb.Span{parent, child} {
					method := attr(s, key(t, "method"))
					if attr(s, key(t, "methodName")) != method {
						t.Fatalf("standard method lost: %v", s)
					}
					_, target, _ := strings.Cut(s.Name, ":")
					switch method {
					case "tools/call":
						if attr(s, key(t, "toolName")) != target {
							t.Fatalf("standard tool name lost: %v", s)
						}
					case "prompts/get":
						if attr(s, key(t, "promptName")) != target {
							t.Fatalf("standard prompt name lost: %v", s)
						}
					case "resources/read":
						if has(s, key(t, "toolName")) || has(s, key(t, "promptName")) || has(s, "mcp.resource.uri") {
							t.Fatalf("resource named by URI: %v", s)
						}
					}
				}
			}
			lookup := named1(t, transports, "mcp.tools/call:lookup")
			if lookup.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR || has(lookup, key(t, "errorCode")) || has(lookup, key(t, "isError")) {
				t.Fatalf("successful call failed: %v", lookup)
			}
			failure := named1(t, transports, "mcp.tools/call:failure")
			if failure.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || !boolValue(failure, key(t, "isError")) || attr(failure, "error.type") != "tool_error" {
				t.Fatalf("IsError lost: %v", failure)
			}
			lowlevel := named1(t, transports, "mcp.tools/call:lowlevel")
			if code, ok := intValue(lowlevel, key(t, "errorCode")); !ok || code != jsonrpc.CodeInvalidParams || lowlevel.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || attr(lowlevel, "error.type") != "McpJsonRpcError" {
				t.Fatalf("JSON-RPC error code lost: %v", lowlevel)
			}
			if strings.Contains(fmtSpans(got), "private-") {
				t.Fatalf("content sent with payload capture off: %s", fmtSpans(got))
			}
		})
	}
}

func fmtSpans(got []*tracepb.Span) string {
	var b strings.Builder
	for _, s := range got {
		b.WriteString(s.String())
	}
	return b.String()
}

func rawRequest(t *testing.T, id int, method string, params any) *jsonrpc.Request {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	r := &jsonrpc.Request{Method: method, Params: raw}
	if id > 0 {
		if r.ID, err = jsonrpc.MakeID(float64(id)); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// This fails if a request the SDK refuses before any middleware (here an
// unknown method) produces no transport span, or loses its JSON-RPC code.
func TestRefusedRequestKeepsJSONRPCErrorCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initialize := map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "raw-client", "version": "1"}}
	t.Run("memory", func(t *testing.T) {
		c, receiver := newCapture(t)
		server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
		conn, err := serve(t, ctx, c, server, "memory").Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.Write(ctx, rawRequest(t, 1, "initialize", initialize)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Read(ctx); err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, rawRequest(t, 0, "notifications/initialized", map[string]any{})); err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, rawRequest(t, 2, "unknown/method", map[string]any{})); err != nil {
			t.Fatal(err)
		}
		msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if resp, ok := msg.(*jsonrpc.Response); !ok || resp.Error == nil {
			t.Fatalf("unknown method answered: %+v", msg)
		}
		span := named1(t, spans(t, c, receiver), "mcp.unknown/method")
		if code, ok := intValue(span, key(t, "errorCode")); !ok || code != jsonrpc.CodeMethodNotFound || span.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR {
			t.Fatalf("refused request lost its code: %v", span)
		}
	})
	t.Run("stateless", func(t *testing.T) {
		c, receiver := newCapture(t)
		server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
		app := httptest.NewServer(agentrailmcp.Handler(c, sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})))
		defer app.Close()
		body, err := jsonrpc.EncodeMessage(rawRequest(t, 7, "unknown/method", map[string]any{}))
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, app.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
		span := named1(t, spans(t, c, receiver), "mcp.unknown/method")
		if code, ok := intValue(span, key(t, "errorCode")); !ok || code != jsonrpc.CodeMethodNotFound {
			t.Fatalf("refused request lost its code: %v", span)
		}
		if n, ok := intValue(span, key(t, "responseBytes")); !ok || n <= 0 {
			t.Fatalf("refusal size lost: %v", span)
		}
	})
}

// This fails if a JSON-RPC error code outside 32 bits is sent, which makes
// the collector refuse the whole export, or if the span stops being an error.
func TestOutOfRangeErrorCodeIsOmitted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, receiver := newCapture(t)
	server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
	for name, code := range map[string]int64{"wide": 2147483648, "lowest": -2147483648} {
		server.AddTool(&sdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return nil, &jsonrpc.Error{Code: code, Message: "synthetic"}
		})
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "synthetic-caller", Version: "2.3"}, nil)
	session, err := client.Connect(ctx, serve(t, ctx, c, server, "memory"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, name := range []string{"wide", "lowest"} {
		if _, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: map[string]any{}}); err == nil {
			t.Fatalf("%s: error swallowed", name)
		}
	}
	got := spans(t, c, receiver)
	wide := named1(t, got, "mcp.tools/call:wide")
	if has(wide, key(t, "errorCode")) || wide.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || attr(wide, "error.type") != "McpJsonRpcError" {
		t.Fatalf("out-of-range code: %v", wide)
	}
	if code, ok := intValue(named1(t, got, "mcp.tools/call:lowest"), key(t, "errorCode")); !ok || code != -2147483648 {
		t.Fatalf("in-range code lost: %d, %v", code, ok)
	}
}

// This fails if a resources/read span name carries the requested URI's
// userinfo, query or fragment, where credentials and tokens travel.
func TestResourceSpanNameDropsCredentialsQueryAndFragment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, receiver := newCapture(t)
	server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
	client := sdk.NewClient(&sdk.Implementation{Name: "synthetic-caller", Version: "2.3"}, nil)
	session, err := client.Connect(ctx, serve(t, ctx, c, server, "memory"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, uri := range []string{"https://user:private-secret@host.example/docs/a?token=private-token#private-part", "file:///notes/b.txt?private-q", "urn:x:private-c#private-d"} {
		if _, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err == nil {
			t.Fatalf("%s: unknown resource read", uri)
		}
	}
	got := spans(t, c, receiver)
	for _, name := range []string{"mcp.resources/read:https://host.example/docs/a", "mcp.resources/read:file:///notes/b.txt", "mcp.resources/read:urn:x:private-c"} {
		named1(t, got, name)
	}
	for _, s := range got {
		if strings.Contains(s.Name, "private-secret") || strings.Contains(s.Name, "private-token") || strings.Contains(s.Name, "private-q") || strings.Contains(s.Name, "private-d") {
			t.Fatalf("span name %q", s.Name)
		}
	}
}

// rawPost sends one JSON-RPC message to a Streamable HTTP endpoint and
// returns the status and the session ID the answer issued.
func rawPost(t *testing.T, ctx context.Context, endpoint, session, protocol string, id int, method string, params any) (int, string) {
	t.Helper()
	body, err := jsonrpc.EncodeMessage(rawRequest(t, id, method, params))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("Mcp-Protocol-Version", protocol)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id")
}

// This fails if Handler hashes a session ID only the request carried (in
// stateless mode), prefers the version
// initialize said over the one a message carries, or leaves a request refused
// without a JSON-RPC answer with no McpTransportRejected error type.
func TestHTTPSpansDescribeTheServerSessionAndTheMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initialize := map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "raw-client", "version": "1"}}
	endpoint := func(t *testing.T, stateless bool) (*agentrail.Client, *capture, string) {
		c, receiver := newCapture(t)
		server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
		app := httptest.NewServer(agentrailmcp.Handler(c, sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: stateless})))
		t.Cleanup(app.Close)
		return c, receiver, app.URL
	}
	t.Run("stateless", func(t *testing.T) {
		c, receiver, url := endpoint(t, true)
		if status, _ := rawPost(t, ctx, url, "client-made-id", "2025-11-25", 1, "tools/list", map[string]any{}); status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		span := named1(t, spans(t, c, receiver), "mcp.tools/list")
		if has(span, key(t, "sessionHash")) || attr(span, key(t, "protocolVersion")) != "2025-11-25" {
			t.Fatalf("stateless span: %v", span)
		}
	})
	t.Run("stateful", func(t *testing.T) {
		c, receiver, url := endpoint(t, false)
		status, issued := rawPost(t, ctx, url, "", "", 1, "initialize", initialize)
		if status != http.StatusOK || issued == "" {
			t.Fatalf("initialize: status %d, session %q", status, issued)
		}
		rawPost(t, ctx, url, issued, "2025-11-25", 0, "notifications/initialized", map[string]any{})
		rawPost(t, ctx, url, issued, "2025-06-18", 2, "tools/list", map[string]any{})
		// Before 2026-07-28 the SDK refuses an unknown method in plain text.
		if status, _ := rawPost(t, ctx, url, issued, "2025-11-25", 3, "unknown/method", map[string]any{}); status != http.StatusBadRequest {
			t.Fatalf("unknown method: status %d", status)
		}
		got := spans(t, c, receiver)
		list := named1(t, got, "mcp.tools/list")
		if attr(list, key(t, "sessionHash")) != handoff.Hash("synthetic-key", issued) || attr(list, key(t, "protocolVersion")) != "2025-06-18" {
			t.Fatalf("issued session or carried version lost: %v", list)
		}
		refused := named1(t, got, "mcp.unknown/method")
		if has(refused, key(t, "errorCode")) || refused.Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || attr(refused, "error.type") != "McpTransportRejected" {
			t.Fatalf("refused request: %v", refused)
		}
	})
}

// This fails if payloads are sent when the redact hook drops them, if the
// hook's result is not what is sent, if a panicking hook breaks the call, or
// if a hook that panics or returns invalid JSON is not reported as
// payload_redact_failed.
func TestPayloadsFollowTheRedactHook(t *testing.T) {
	for _, mode := range []string{"memory", "http", "sse"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var mu sync.Mutex
			codes := []string{}
			c, receiver := newCaptureWith(t, func(o *agentrail.Options) {
				o.OnError = func(_ context.Context, e agentrail.CaptureError) {
					mu.Lock()
					codes = append(codes, e.Code)
					mu.Unlock()
				}
				o.MCP = agentrail.MCPOptions{CapturePayloads: true, Redact: func(p agentrail.MCPPayload) (agentrail.MCPPayload, bool) {
					switch p.Name {
					case "dropped":
						return p, false
					case "panics":
						panic("private-redact-panic")
					case "invalid":
						p.Response = json.RawMessage(`{"private-invalid"`)
						return p, true
					}
					p.Request = bytes.ReplaceAll(p.Request, []byte("private-argument"), []byte("[redacted]"))
					return p, true
				}}
			})
			server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
			for _, name := range []string{"kept", "dropped", "panics", "invalid"} {
				sdk.AddTool(server, &sdk.Tool{Name: name}, agentrailmcp.Tool(c, name, func(ctx context.Context, req *sdk.CallToolRequest, in input) (*sdk.CallToolResult, output, error) {
					return nil, output{Value: "visible-result"}, nil
				}))
			}
			client := sdk.NewClient(&sdk.Implementation{Name: "synthetic-caller", Version: "2.3"}, nil)
			session, err := client.Connect(ctx, serve(t, ctx, c, server, mode), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			for _, name := range []string{"kept", "dropped", "panics", "invalid"} {
				if r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: map[string]any{"secret": "private-argument"}}); err != nil || r.IsError {
					t.Fatalf("%s changed by capture: %+v, %v", name, r, err)
				}
			}
			got := spans(t, c, receiver)
			transports := byLayer(t, got, "transport")
			kept := named1(t, transports, "mcp.tools/call:kept")
			request, response := attr(kept, key(t, "requestPayload")), attr(kept, key(t, "responsePayload"))
			if !json.Valid([]byte(request)) || !strings.Contains(request, `"[redacted]"`) || !strings.Contains(request, `"name":"kept"`) || !strings.Contains(response, "visible-result") {
				t.Fatalf("payload is not what the hook returned: %q / %q", request, response)
			}
			for _, name := range []string{"dropped", "panics", "invalid"} {
				s := named1(t, transports, "mcp.tools/call:"+name)
				if has(s, key(t, "requestPayload")) || has(s, key(t, "responsePayload")) {
					t.Fatalf("%s payload sent: %v", name, s)
				}
			}
			for _, s := range byLayer(t, got, "handler") {
				if has(s, key(t, "requestPayload")) || has(s, key(t, "responsePayload")) {
					t.Fatalf("handler carries a payload: %v", s)
				}
			}
			if strings.Contains(fmtSpans(got), "private-") {
				t.Fatalf("redacted text sent")
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(codes, ",") != "payload_redact_failed,payload_redact_failed" {
				t.Fatalf("diagnostics %v", codes)
			}
		})
	}
}

// This fails if a result too large for one export is sent whole (the export
// fails and the span is lost) instead of being cut, as valid UTF-8, so that
// both payloads together fill the contract's limits.mcpPayloadBytes.
func TestOversizedResultIsTruncatedToFitOneExport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var mu sync.Mutex
	codes := []string{}
	c, receiver := newCaptureWith(t, func(o *agentrail.Options) {
		o.OnError = func(_ context.Context, e agentrail.CaptureError) {
			mu.Lock()
			codes = append(codes, e.Code)
			mu.Unlock()
		}
		o.MCP = agentrail.MCPOptions{CapturePayloads: true, Redact: func(p agentrail.MCPPayload) (agentrail.MCPPayload, bool) { return p, true }}
	})
	limit := contract(t).OTLP.Limits.MCPPayloadBytes
	large := strings.Repeat("é", limit/2+1024) // over the limit in bytes, two per rune
	server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "large"}, agentrailmcp.Tool(c, "large", func(ctx context.Context, req *sdk.CallToolRequest, in input) (*sdk.CallToolResult, output, error) {
		return nil, output{Value: large}, nil
	}))
	client := sdk.NewClient(&sdk.Implementation{Name: "synthetic-caller", Version: "2.3"}, nil)
	// The in-memory transport limits frames; Streamable HTTP does not.
	session, err := client.Connect(ctx, serve(t, ctx, c, server, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "large", Arguments: map[string]any{"secret": "small"}}); err != nil || r.IsError {
		t.Fatalf("large call failed: %v", err)
	}
	span := named1(t, byLayer(t, spans(t, c, receiver), "transport"), "mcp.tools/call:large")
	response, request := attr(span, key(t, "responsePayload")), attr(span, key(t, "requestPayload"))
	// Together within the contract's limit, the longer cut to fill it.
	if total := len(request) + len(response); !boolValue(span, key(t, "responseTruncated")) || has(span, key(t, "requestTruncated")) || total > limit || total < limit-1 || !utf8.ValidString(response) || !strings.Contains(request, "small") {
		t.Fatalf("not cut to fit: %d bytes, truncated=%v", len(response), boolValue(span, key(t, "responseTruncated")))
	}
	if n, _ := intValue(span, key(t, "responseBytes")); int(n) <= limit {
		t.Fatalf("response size is the cut size: %d", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(codes) != 0 {
		t.Fatalf("diagnostics %v", codes)
	}
}
