// Package mcp records MCP traffic of official MCP Go SDK servers as
// Agentrail evidence.
//
// Handler (for StreamableHTTPHandler and SSEHandler) and Transport (for a
// transport passed to Server.Connect) record one transport span per JSON-RPC
// message the server receives: its method, the tool, prompt or resource it
// names, client metadata, a keyed hash of the session ID, message sizes, and
// JSON-RPC errors and IsError results. Arguments, results and resource content
// are sent only when Options.MCP.CapturePayloads is set, after its redact hook.
//
// Tool, ToolHandler, Resource and Prompt wrap registered callbacks. Inside a
// transport span the callback's span is its child and no step of its own;
// without a transport wrapper it is the call's step, as before. Client-reported
// metadata is descriptive and is never authenticated identity.
package mcp

import (
	"context"
	"strings"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/mcpspan"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type requestMetadata interface {
	ClientInfo() *sdk.Implementation
	ProtocolVersion() string
	GetExtra() *sdk.RequestExtra
	GetSession() sdk.Session
}

// observe runs a registered callback in its handler span. target is the
// tool or prompt name, or resource URI, the request carried; label is the
// stable registration name the span is named after.
func observe(ctx context.Context, client *agentrail.Client, req requestMetadata, method, target, label string, run func(context.Context) error) error {
	parent, active := lookup(ctx, req.GetExtra(), method, target)
	meta := mcpspan.Meta{Protocol: req.ProtocolVersion(), RegisteredName: label}
	if c := req.ClientInfo(); c != nil {
		meta.Client, meta.ClientVersion = c.Name, c.Version
	}
	// A request built outside a session (a direct call in tests) has none.
	if s, ok := req.GetSession().(*sdk.ServerSession); ok && s != nil {
		meta.SessionID = s.ID()
	}
	return mcpspan.Handler(client, ctx, parent, active, method, spanName(method, label), meta, run)
}

// spanName uses the shared MCP operation vocabulary and fits the milestone
// label limit in UTF-16 units without cutting a Unicode code point.
func spanName(method, name string) string {
	label := "mcp." + method + ":" + strings.ReplaceAll(strings.ToValidUTF8(name, "\ufffd"), "\x00", "")
	units := 0
	for i, r := range label {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > 256 {
			return label[:i]
		}
		units += width
	}
	return label
}

// Tool wraps a typed handler for sdk.AddTool. name must be a stable name chosen
// at registration, never a value from arguments. The original result, output,
// error, and panic are preserved. Both errors and IsError results mark failure.
// Span names are mcp.tools/call:<name>, bounded to 256 UTF-16 units.
func Tool[In, Out any](client *agentrail.Client, name string, next sdk.ToolHandlerFor[In, Out]) sdk.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (result *sdk.CallToolResult, out Out, err error) {
		observe(ctx, client, req, "tools/call", toolName(req), name, func(ctx context.Context) error {
			result, out, err = next(ctx, req, in)
			if err != nil {
				return err
			}
			if result != nil && result.IsError {
				return mcpspan.OperationError{}
			}
			return nil
		})
		return
	}
}

// ToolHandler wraps the low-level handler accepted by Server.AddTool, retaining
// its protocol-error semantics and marking IsError results as failures.
func ToolHandler(client *agentrail.Client, name string, next sdk.ToolHandler) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (result *sdk.CallToolResult, err error) {
		observe(ctx, client, req, "tools/call", toolName(req), name, func(ctx context.Context) error {
			result, err = next(ctx, req)
			if err != nil {
				return err
			}
			if result != nil && result.IsError {
				return mcpspan.OperationError{}
			}
			return nil
		})
		return
	}
}

// Resource wraps the handler accepted by Server.AddResource or
// Server.AddResourceTemplate. name must be a stable registration label, never
// the requested URI. Results, errors, and panics retain their original values.
func Resource(client *agentrail.Client, name string, next sdk.ResourceHandler) sdk.ResourceHandler {
	return func(ctx context.Context, req *sdk.ReadResourceRequest) (result *sdk.ReadResourceResult, err error) {
		target := ""
		if req != nil && req.Params != nil {
			target = req.Params.URI
		}
		err = observe(ctx, client, req, "resources/read", target, name, func(ctx context.Context) error { result, err = next(ctx, req); return err })
		return
	}
}

func toolName(req *sdk.CallToolRequest) string {
	if req == nil || req.Params == nil {
		return ""
	}
	return req.Params.Name
}

// Prompt wraps the handler accepted by Server.AddPrompt. name must be a stable
// registration label. The wrapper itself never captures arguments or messages.
func Prompt(client *agentrail.Client, name string, next sdk.PromptHandler) sdk.PromptHandler {
	return func(ctx context.Context, req *sdk.GetPromptRequest) (result *sdk.GetPromptResult, err error) {
		target := ""
		if req != nil && req.Params != nil {
			target = req.Params.Name
		}
		err = observe(ctx, client, req, "prompts/get", target, name, func(ctx context.Context) error { result, err = next(ctx, req); return err })
		return
	}
}
