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
	// ID is the JSON-RPC id as text, a number in decimal; empty for a
	// notification.
	ID string
	// HTTP is the request that carried the message, shared by every message
	// of a batch; nil when it came by any other transport.
	HTTP *Request
}

// Request is what an HTTP request that carried MCP messages said of itself.
type Request struct {
	// Method is the request's method; URL is the request URL as received,
	// before its query is cut to the marker lane.
	Method, URL, UserAgent string
	// HeaderNames are the names of the request's headers, lower-cased and
	// sorted: net/http keeps neither their case nor their order.
	HeaderNames []string
	// Accept and AcceptEncoding are the headers' values as sent, one per
	// line; nil when the request sent none.
	Accept, AcceptEncoding []string
}

// Meta is what the client said about itself, and the session it is in.
type Meta struct {
	Client, ClientVersion, Protocol, SessionID string
	// RegisteredName names a handler independently of its bounded span label.
	RegisteredName string
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
	// Status is the HTTP status of the response that answered or refused the
	// call, when that is the response to the request that carried it; 0
	// otherwise.
	Status int
}

// Span is a transport span that is open until its call is answered.
type Span interface {
	End(Outcome)
	// Graft returns ctx carrying this span as the parent of new spans.
	Graft(ctx context.Context) context.Context
	// Threw records that a handler run inside this call returned an error or
	// panicked (not an IsError result), with the error type its own span
	// carries: the call is a failed call, whatever the server answered.
	Threw(errorType string)
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
