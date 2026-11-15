package mcp_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	agentrail "github.com/agentrail-co/agentrail-go"
	agentrailmcp "github.com/agentrail-co/agentrail-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const listProjectsCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`

// listProjects is a stateless MCP endpoint with one tool, behind Middleware
// and Handler. The recorded requests name a stand-in host, so the SDK's
// loopback Host check is off.
func listProjects(c *agentrail.Client) http.Handler {
	server := sdk.NewServer(&sdk.Implementation{Name: "synthetic-server", Version: "1.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "list_projects"}, agentrailmcp.Tool(c, "list_projects", func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "[]"}}}, nil, nil
	}))
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, DisableLocalhostProtection: true})
	return c.Middleware(agentrailmcp.Handler(c, handler))
}

// writeRaw sends request bytes as they are, which net/http's client would
// reorder and add to, and returns the response status.
func writeRaw(t *testing.T, conn net.Conn, request []byte) int {
	t.Helper()
	defer conn.Close()
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// replay writes a recorded request as its client wrote it: its header names
// in their order and case, over TLS, as https in its URL says.
func replay(t *testing.T, app *httptest.Server, r recordedRequest) int {
	t.Helper()
	var body bytes.Buffer
	if err := json.Compact(&body, r.Body); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" {
		t.Fatalf("recorded URL %q", r.URL)
	}
	var request bytes.Buffer
	fmt.Fprintf(&request, "%s %s HTTP/1.1\r\n", r.Method, u.RequestURI())
	for _, h := range r.Headers {
		if strings.EqualFold(h[0], "content-length") && h[1] != strconv.Itoa(body.Len()) {
			t.Fatalf("recorded content-length %s for a %d-byte body", h[1], body.Len())
		}
		fmt.Fprintf(&request, "%s: %s\r\n", h[0], h[1])
	}
	request.WriteString("\r\n")
	request.Write(body.Bytes())
	roots := x509.NewCertPool()
	roots.AddCert(app.Certificate())
	conn, err := tls.Dial("tcp", app.Listener.Addr().String(), &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	return writeRaw(t, conn, request.Bytes())
}

func listProjectsSpan(t *testing.T, c *agentrail.Client, r *capture) *tracepb.Span {
	t.Helper()
	return named1(t, byLayer(t, spans(t, c, r), "transport"), "mcp.tools/call:list_projects")
}

// This fails if a transport span's request facts differ from the contract's
// for either recorded client: host left out of the names (net/http keeps it
// in Request.Host, not Header), a header value or a name the client did not
// send among them, Accept-Encoding sent where Codex sent none, the method,
// URL, user agent, Accept, JSON-RPC id or tool changed, or no HTTP status.
// The names compare as a set: Go's http.Header keeps neither their order nor
// their case, so the SDK sends them lower-cased and sorted.
func TestRequestFactsOfTheRecordedClients(t *testing.T) {
	facts := requestFactKeys(t)
	for _, recorded := range contract(t).OTLP.MCPRequestFacts.Requests {
		t.Run(recorded.Name, func(t *testing.T) {
			c, receiver := newCapture(t)
			app := httptest.NewTLSServer(listProjects(c))
			t.Cleanup(app.Close)
			if status := replay(t, app, recorded); status != http.StatusOK {
				t.Fatalf("status %d", status)
			}
			span := listProjectsSpan(t, c, receiver)
			for k := range facts {
				want, sent := recorded.Attributes[k]
				switch {
				case k == "http.response.status_code":
					if code, ok := intValue(span, k); !ok || code != http.StatusOK {
						t.Fatalf("status code %d, %v", code, ok)
					}
				case !sent:
					if has(span, k) {
						t.Fatalf("%s sent as %q; the client sent none", k, attr(span, k))
					}
				case k == key(t, "headerNames"):
					got := strings.Split(attr(span, k), ",")
					wantNames := strings.Split(strings.ToLower(want), ",")
					slices.Sort(wantNames)
					if !slices.IsSorted(got) || !slices.Equal(got, wantNames) {
						t.Fatalf("header names %q, want the set %q", got, wantNames)
					}
				default:
					if got := attr(span, k); got != want {
						t.Fatalf("%s is %q, want %q", k, got, want)
					}
				}
			}
		})
	}
}

// This fails if a name net/http moves out of Header is not added back when
// the client sent it (Host of an HTTP/1.1 request, Transfer-Encoding,
// Trailer), or is added when it did not: an HTTP/2 request names its host in
// :authority, which is no header field.
func TestHeaderNamesNetHTTPKeepsAside(t *testing.T) {
	t.Run("HTTP/1.1 chunked", func(t *testing.T) {
		c, receiver := newCapture(t)
		app := httptest.NewServer(listProjects(c))
		t.Cleanup(app.Close)
		conn, err := net.Dial("tcp", app.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		request := "POST /mcp HTTP/1.1\r\nHost: mcp.example.test\r\nContent-Type: application/json\r\n" +
			"Accept: application/json, text/event-stream\r\nMcp-Protocol-Version: 2025-06-18\r\n" +
			"Transfer-Encoding: chunked\r\nTrailer: X-Checksum\r\n\r\n" +
			fmt.Sprintf("%x\r\n%s\r\n0\r\nX-Checksum: 1\r\n\r\n", len(listProjectsCall), listProjectsCall)
		if status := writeRaw(t, conn, []byte(request)); status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		got := attr(listProjectsSpan(t, c, receiver), key(t, "headerNames"))
		if want := "accept,content-type,host,mcp-protocol-version,trailer,transfer-encoding"; got != want {
			t.Fatalf("header names %q, want %q", got, want)
		}
	})
	t.Run("HTTP/2", func(t *testing.T) {
		c, receiver := newCapture(t)
		app := httptest.NewUnstartedServer(listProjects(c))
		app.EnableHTTP2 = true
		app.StartTLS()
		t.Cleanup(app.Close)
		req, err := http.NewRequest(http.MethodPost, app.URL+"/mcp", strings.NewReader(listProjectsCall))
		if err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Mcp-Protocol-Version": "2025-06-18", "User-Agent": "synthetic-agent/1", "Accept-Encoding": "identity"} {
			req.Header.Set(name, v)
		}
		resp, err := app.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s, status %d", resp.Proto, resp.StatusCode)
		}
		names := strings.Split(attr(listProjectsSpan(t, c, receiver), key(t, "headerNames")), ",")
		if slices.Contains(names, "host") {
			t.Fatalf("host named on an HTTP/2 request that sent none: %q", names)
		}
		for _, sent := range []string{"accept", "accept-encoding", "content-type", "mcp-protocol-version", "user-agent"} {
			if !slices.Contains(names, sent) {
				t.Fatalf("%s missing from %q", sent, names)
			}
		}
	})
}

// This fails if the names list is cut inside a name, which would send a name
// the client never sent, or is not bounded at all.
func TestHeaderNamesStopAtAWholeName(t *testing.T) {
	c, receiver := newCapture(t)
	app := httptest.NewServer(listProjects(c))
	t.Cleanup(app.Close)
	req, err := http.NewRequest(http.MethodPost, app.URL+"/mcp", strings.NewReader(listProjectsCall))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	sent := map[string]bool{"accept": true, "accept-encoding": true, "content-length": true, "content-type": true, "host": true, "mcp-protocol-version": true, "user-agent": true}
	// Enough names to pass the 4096 characters sent (400 of 13 characters
	// each), and few enough that the server takes the request: from Go 1.27
	// net/http answers 431 past 500 header values (DefaultMaxHeaderValueCount).
	for i := range 400 {
		name := fmt.Sprintf("x-filler-%03d", i)
		req.Header.Set(name, "1")
		sent[name] = true
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := attr(listProjectsSpan(t, c, receiver), key(t, "headerNames"))
	names := strings.Split(got, ",")
	if len(got) > 4096 || len(got) < 4096-len("x-filler-000,") || len(names) >= len(sent) {
		t.Fatalf("%d names in %d characters", len(names), len(got))
	}
	for _, name := range names {
		if !sent[name] {
			t.Fatalf("%q is no name the client sent", name)
		}
	}
}

// headerName is an HTTP field name as RFC 9110 defines a token, lower-cased.
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9a-z-]+$")

// This fails if a header value reaches the names attribute, for instance a
// header's values listed beside its name, or a query value other than a
// marker reaches url.full. Of the request, only the method, URL, user agent,
// Accept and Accept-Encoding are sent as values.
func TestRequestFactsCarryNoHeaderOrQueryValue(t *testing.T) {
	c, receiver := newCapture(t)
	app := httptest.NewServer(listProjects(c))
	t.Cleanup(app.Close)
	req, err := http.NewRequest(http.MethodPost, app.URL+"/mcp?token=private-query", strings.NewReader(listProjectsCall))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	req.Header.Set("Authorization", "Bearer private-token")
	// A value that reads as a name once split on its comma.
	req.Header.Set("X-Note", "private-note,x-looks-like-a-name")
	req.Header.Add("Cookie", "private-cookie=private-cookie-value")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := spans(t, c, receiver)
	span := named1(t, byLayer(t, got, "transport"), "mcp.tools/call:list_projects")
	names := strings.Split(attr(span, key(t, "headerNames")), ",")
	for _, name := range names {
		if !headerName.MatchString(name) || name == "x-looks-like-a-name" {
			t.Fatalf("%q in the header names %q is no name the client sent", name, names)
		}
	}
	if !slices.Contains(names, "authorization") || !slices.Contains(names, "x-note") || !slices.Contains(names, "cookie") {
		t.Fatalf("header names %q", names)
	}
	if u := attr(span, "url.full"); u != app.URL+"/mcp" {
		t.Fatalf("url.full %q", u)
	}
	if strings.Contains(fmtSpans(got), "private-") {
		t.Fatalf("a header or query value was sent: %s", fmtSpans(got))
	}
}
