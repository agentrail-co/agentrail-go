package mcp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func registered() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	n := 0
	for _, list := range registry.calls {
		n += len(list)
	}
	return n
}

func internalClient(t *testing.T) *agentrail.Client {
	t.Helper()
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body) }))
	t.Cleanup(collector.Close)
	c, err := agentrail.New(context.Background(), agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: collector.URL, Secret: "synthetic-source"}, HashKey: "synthetic-key", ServiceName: "mcp-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	return c
}

func postBody(t *testing.T, ctx context.Context, url string, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// This fails if a request whose JSON-RPC id repeats one still pending
// replaces it without ending it, so the first call stays registered forever
// and a later request reusing the freed header map can claim it.
func TestRepeatedIDsLeaveNothingRegistered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := internalClient(t)
	server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
	t.Run("http batch", func(t *testing.T) {
		before := registered()
		app := httptest.NewServer(Handler(c, sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})))
		defer app.Close()
		batch := []byte(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`)
		for range 20 {
			postBody(t, ctx, app.URL, batch)
		}
		if n := registered() - before; n != 0 {
			t.Fatalf("%d calls left registered", n)
		}
	})
	t.Run("connection", func(t *testing.T) {
		before := registered()
		st, ct := sdk.NewInMemoryTransports()
		session, err := server.Connect(ctx, Transport(c, st), nil)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := ct.Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		zero, _ := jsonrpc.MakeID(float64(0))
		if err := conn.Write(ctx, &jsonrpc.Request{ID: zero, Method: "initialize", Params: []byte(`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Read(ctx); err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, &jsonrpc.Request{Method: "notifications/initialized", Params: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		id, _ := jsonrpc.MakeID(float64(1))
		for range 2 {
			if err := conn.Write(ctx, &jsonrpc.Request{ID: id, Method: "tools/list"}); err != nil {
				t.Fatal(err)
			}
		}
		// The SDK answers one of the two; the other is never answered.
		if _, err := conn.Read(ctx); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		session.Close()
		session.Wait()
		if n := registered() - before; n != 0 {
			t.Fatalf("%d calls left registered", n)
		}
	})
}

// This fails if a handler that panics leaves its request's calls registered.
func TestPanickingHandlerLeavesNothingRegistered(t *testing.T) {
	c := internalClient(t)
	before := registered()
	h := Handler(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		panic("synthetic")
	}))
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	if n := registered() - before; n != 0 {
		t.Fatalf("%d calls left registered", n)
	}
}
