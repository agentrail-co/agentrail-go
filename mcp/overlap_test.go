package mcp_test

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"go.opentelemetry.io/otel/trace"
)

type numbered struct {
	N int `json:"n"`
}

// This fails if a callback can be given another request's transport span:
// two legacy SSE calls to one tool, registered in one order and reaching
// their callbacks in the other, must never cross parents.
func TestOverlappingSSECallsNeverCrossParents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, receiver := newCaptureWith(t, func(o *agentrail.Options) {
		o.MCP = agentrail.MCPOptions{CapturePayloads: true, Redact: func(p agentrail.MCPPayload) (agentrail.MCPPayload, bool) { return p, true }}
	})
	firstRead, secondEntered, firstEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	handlerOf := map[string]int{} // handler span ID -> the n its own request carried
	server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "same"}, agentrailmcp.Tool(c, "same", func(ctx context.Context, req *sdk.CallToolRequest, in numbered) (*sdk.CallToolResult, output, error) {
		id := trace.SpanContextFromContext(ctx).SpanID()
		mu.Lock()
		handlerOf[hex.EncodeToString(id[:])] = in.N
		mu.Unlock()
		if in.N == 2 {
			close(secondEntered)
			<-firstEntered // still pending while the first callback looks up
		} else {
			close(firstEntered)
		}
		return nil, output{}, nil
	}))
	sse := sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return server }, nil)
	// The first call's message is read (and registered) first but reaches
	// the server only after the second call's callback has started.
	delay := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"n":1`)) {
				close(firstRead)
				<-secondEntered
			}
		}
		sse.ServeHTTP(w, r)
	})
	app := httptest.NewServer(agentrailmcp.Handler(c, delay))
	defer app.Close()
	client := sdk.NewClient(&sdk.Implementation{Name: "synthetic-caller", Version: "2.3"}, nil)
	session, err := client.Connect(ctx, &sdk.SSEClientTransport{Endpoint: app.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var wg sync.WaitGroup
	for n := 1; n <= 2; n++ {
		if n == 2 {
			<-firstRead
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "same", Arguments: map[string]any{"n": n}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got := spans(t, c, receiver)
	transports := map[string]string{} // transport span ID -> its request payload
	for _, s := range byLayer(t, got, "transport") {
		transports[hex.EncodeToString(s.SpanId)] = attr(s, key(t, "requestPayload"))
	}
	handlers := byLayer(t, got, "handler")
	if len(handlers) != 2 || len(transports) < 2 {
		t.Fatalf("%d handler spans, %d transport spans", len(handlers), len(transports))
	}
	for _, h := range handlers {
		n, ok := handlerOf[hex.EncodeToString(h.SpanId)]
		if !ok {
			t.Fatalf("unknown handler span %v", h)
		}
		if len(h.ParentSpanId) == 0 {
			continue // not attributable: no parent rather than a wrong one
		}
		if payload := transports[hex.EncodeToString(h.ParentSpanId)]; !strings.Contains(payload, `"n":`+string(rune('0'+n))) {
			t.Fatalf("callback for n=%d parented to the transport span of %q", n, payload)
		}
		if has(h, "agentrail.step") {
			t.Fatalf("handler span became a step: %v", h)
		}
	}
}
