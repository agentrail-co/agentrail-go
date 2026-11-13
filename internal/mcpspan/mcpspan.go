// Package mcpspan connects the MCP adapter to the client's private span
// recording. The root package installs the functions; the mcp package calls
// them. Nothing here is part of the public API.
package mcpspan

import (
	"context"
	"encoding/json"
)

// Call is one JSON-RPC request or notification the server received.
type Call struct {
	// Method is the JSON-RPC method as received.
	Method string
	// Target is the tool or prompt name, or the resource URI, for tools/call,
	// prompts/get and resources/read; empty otherwise.
	Target string
	// RequestBytes is the length of the message as the MCP SDK encodes it, or
	// -1 when it could not be encoded.
	RequestBytes int
	// Params is the request's params JSON, used only for opt-in payloads.
	Params json.RawMessage
}

// Meta is what the client said about itself, and the session it is in.
type Meta struct {
	Client, ClientVersion, Protocol, SessionID string
}

// Outcome is how the server answered one call.
type Outcome struct {
	Meta
	// Responded is true when the server wrote a JSON-RPC response.
	Responded     bool
	ResponseBytes int
	// ErrorCode is set for a JSON-RPC error response.
	ErrorCode *int64
	// IsError is a tools/call result that said it failed.
	IsError bool
	// Result is the response's result, or its error object, as JSON.
	Result json.RawMessage
	// Rejected is a request the transport refused without a JSON-RPC answer.
	Rejected bool
}

// Span is a transport span that is open until its call is answered.
type Span interface {
	End(Outcome)
	// Graft returns ctx carrying this span as the parent of new spans.
	Graft(ctx context.Context) context.Context
}

// OperationError marks a tool result whose IsError is true.
type OperationError struct{}

func (OperationError) Error() string { return "MCP tool reported failure" }

// Transport starts the transport span of a received message. client is the
// *agentrail.Client.
var Transport func(client any, ctx context.Context, call Call) Span

// Handler runs a tool, resource or prompt callback in its handler span:
// a child of parent when one is given; no step when active (a transport
// wrapper sees the call); otherwise the call's step.
var Handler func(client any, ctx context.Context, parent Span, active bool, method, name string, meta Meta, run func(context.Context) error) error
