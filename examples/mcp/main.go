package main

import (
	"context"
	"log"
	"net/http"
	"os"

	agentrail "github.com/agentrail-co/agentrail-go"
	capturemcp "github.com/agentrail-co/agentrail-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type input struct {
	OrderID string `json:"orderId"`
}
type output struct {
	Accepted bool `json:"accepted"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	ctx := context.Background()
	c, err := agentrail.New(ctx, agentrail.Options{
		Collector:   agentrail.CollectorOptions{Endpoint: os.Getenv("AGENTRAIL_ENDPOINT"), Secret: os.Getenv("AGENTRAIL_SOURCE_SECRET")},
		HashKey:     os.Getenv("AGENTRAIL_HASH_KEY"),
		ServiceName: "orders-mcp",
		// Payload capture is off unless set. To send arguments and results, set
		// CapturePayloads and redact what must not leave the server:
		// MCP: agentrail.MCPOptions{CapturePayloads: true, Redact: func(p agentrail.MCPPayload) (agentrail.MCPPayload, bool) { return p, p.Name != "confirm_order" }},
	})
	if err != nil {
		return err
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "orders", Version: "example"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "confirm_order", Description: "Confirm an order"}, capturemcp.Tool(c, "confirm_order", func(ctx context.Context, req *sdk.CallToolRequest, in input) (*sdk.CallToolResult, output, error) {
		// Authenticate at the application's HTTP/auth boundary before asserting identity.
		return nil, output{Accepted: true}, nil
	}))
	// capturemcp.Handler records every JSON-RPC message the server receives;
	// the tool's own span becomes its child. For stdio or in-memory servers,
	// wrap the transport instead: server.Connect(ctx, capturemcp.Transport(c, transport), nil).
	mcpHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
	_ = &http.Server{Addr: ":8080", Handler: c.Middleware(capturemcp.Handler(c, mcpHandler))}
	// Mount behind the application's auth boundary, then serve.
	return c.Shutdown(ctx)
}
