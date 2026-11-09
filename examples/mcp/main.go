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
	c, err := agentrail.New(ctx, agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: os.Getenv("AGENTRAIL_ENDPOINT"), Secret: os.Getenv("AGENTRAIL_SOURCE_SECRET")}, HashKey: os.Getenv("AGENTRAIL_HASH_KEY"), ServiceName: "orders-mcp"})
	if err != nil {
		return err
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "orders", Version: "example"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "confirm_order", Description: "Confirm an order"}, capturemcp.Tool(c, "confirm_order", func(ctx context.Context, req *sdk.CallToolRequest, in input) (*sdk.CallToolResult, output, error) {
		// Authenticate at the application's HTTP/auth boundary before asserting identity.
		return nil, output{Accepted: true}, nil
	}))
	_ = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
	// Mount behind c.Middleware and the application's auth boundary. Arguments and
	// returned content are never exported by the adapter.
	return c.Shutdown(ctx)
}
