package mcp

import (
	"context"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/mcpspan"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport wraps a transport the application passes to Server.Connect
// (stdio, in-memory, IOTransport, or a StreamableServerTransport it serves
// itself) so every JSON-RPC message the server receives becomes a transport
// span, including requests the SDK refuses before any middleware runs. For
// StreamableHTTPHandler and SSEHandler, which build their transports
// internally, use Handler instead.
//
// Each request is given an empty *sdk.RequestExtra when its transport supplies
// none, which is how a handler wrapper finds its transport span. Like
// sdk.LoggingTransport, the wrapper hides the SDK's private connection hooks,
// so a stdio session no longer refuses JSON-RPC batches in protocol
// 2025-06-18 and later.
func Transport(client *agentrail.Client, next sdk.Transport) sdk.Transport {
	return &transport{client: client, next: next}
}

type transport struct {
	client *agentrail.Client
	next   sdk.Transport
}

func (t *transport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := t.next.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &connection{Connection: conn, tracker: newTracker(t.client, mcpspan.Meta{})}, nil
}

// SupportsProtocolVersion keeps the wrapped transport's protocol limits.
func (t *transport) SupportsProtocolVersion(version string) bool {
	if s, ok := t.next.(sdk.ProtocolVersionSupporter); ok {
		return s.SupportsProtocolVersion(version)
	}
	return true
}

type connection struct {
	sdk.Connection
	tracker *tracker
}

func (c *connection) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		return msg, err
	}
	if req, ok := msg.(*jsonrpc.Request); ok {
		c.tracker.setSession(c.SessionID())
		keys := []any{}
		if req.Extra == nil {
			req.Extra = &sdk.RequestExtra{}
		}
		if extra, ok := req.Extra.(*sdk.RequestExtra); ok {
			keys = append(keys, extra)
		}
		c.tracker.receive(ctx, req, keys...)
	}
	return msg, nil
}

// Write ends a response's transport span before the client can read it.
func (c *connection) Write(ctx context.Context, msg jsonrpc.Message) error {
	c.tracker.setSession(c.SessionID())
	c.tracker.respond(msg)
	return c.Connection.Write(ctx, msg)
}

func (c *connection) Close() error {
	err := c.Connection.Close()
	c.tracker.abandon(false)
	return err
}
