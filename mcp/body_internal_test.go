package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type repeat byte

func (b repeat) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// This fails if Handler reads more of a POST body than the MCP SDK would:
// past the SDK's body limit, or before its cross-origin check refuses it.
func TestHandlerReadsNoMoreThanTheSDK(t *testing.T) {
	c := internalClient(t)
	server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
	const size = 40 << 20
	read := func(h http.Handler, crossSite bool) int64 {
		var n atomic.Int64
		body := io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"`), io.LimitReader(repeat('a'), size), strings.NewReader(`"}}`))
		req := httptest.NewRequest(http.MethodPost, "/", countingReader{body, &n})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if crossSite {
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return n.Load()
	}
	for _, crossSite := range []bool{false, true} {
		sdkHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, CrossOriginProtection: http.NewCrossOriginProtection()})
		bare, wrapped := read(sdkHandler, crossSite), read(Handler(c, sdkHandler), crossSite)
		if wrapped > bare || bare >= size {
			t.Fatalf("cross-site %v: the SDK read %d bytes, with Handler %d", crossSite, bare, wrapped)
		}
	}
}
