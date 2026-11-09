// Package mcp adds opt-in Agentrail milestones around official MCP Go SDK
// handlers. It records stable registered operation names and client/protocol
// metadata, never arguments, resource URIs, prompt text, or returned content.
// Client-reported metadata is descriptive and is never authenticated identity.
package mcp

import (
	"context"
	"strings"

	agentrail "github.com/agentrail-co/agentrail-go"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type requestMetadata interface {
	ClientInfo() *sdk.Implementation
	ProtocolVersion() string
}

func details(req requestMetadata, method, kind string) agentrail.MilestoneContext {
	metadata := &agentrail.MCPContext{Method: method, Protocol: req.ProtocolVersion()}
	if client := req.ClientInfo(); client != nil {
		metadata.Client = client.Name
		metadata.ClientVersion = client.Version
	}
	return agentrail.MilestoneContext{Surface: agentrail.SurfaceMCP, Kind: kind, MCP: metadata}
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

type toolResultError struct{}

func (toolResultError) Error() string { return "MCP tool reported failure" }

// Tool wraps a typed handler for sdk.AddTool. name must be a stable name chosen
// at registration, never a value from arguments. The original result, output,
// error, and panic are preserved. Both errors and IsError results mark failure.
// Span names are mcp.tools/call:<name>, bounded to 256 UTF-16 units.
func Tool[In, Out any](client *agentrail.Client, name string, next sdk.ToolHandlerFor[In, Out]) sdk.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (result *sdk.CallToolResult, out Out, err error) {
		client.WithMilestone(ctx, spanName("tools/call", name), details(req, "tools/call", "tool.call"), func(ctx context.Context) error {
			result, out, err = next(ctx, req, in)
			if err != nil {
				return err
			}
			if result != nil && result.IsError {
				return toolResultError{}
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
		client.WithMilestone(ctx, spanName("tools/call", name), details(req, "tools/call", "tool.call"), func(ctx context.Context) error {
			result, err = next(ctx, req)
			if err != nil {
				return err
			}
			if result != nil && result.IsError {
				return toolResultError{}
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
		err = client.WithMilestone(ctx, spanName("resources/read", name), details(req, "resources/read", "mcp.resources/read"), func(ctx context.Context) error { result, err = next(ctx, req); return err })
		return
	}
}

// Prompt wraps the handler accepted by Server.AddPrompt. name must be a stable
// registration label. Arguments and prompt messages are never captured.
func Prompt(client *agentrail.Client, name string, next sdk.PromptHandler) sdk.PromptHandler {
	return func(ctx context.Context, req *sdk.GetPromptRequest) (result *sdk.GetPromptResult, err error) {
		err = client.WithMilestone(ctx, spanName("prompts/get", name), details(req, "prompts/get", "mcp.prompts/get"), func(ctx context.Context) error { result, err = next(ctx, req); return err })
		return
	}
}
