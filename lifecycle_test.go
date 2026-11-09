package agentrail_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	agentrail "github.com/agentrail-co/agentrail-go"
)

func TestClientSpanCoversResponseBody(t *testing.T) {
	_, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "part")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer target.Close()
	defer close(release)
	ctx, end := c.StartMilestone(context.Background(), "request", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "request"})
	defer end(nil)
	request, _ := http.NewRequestWithContext(ctx, "GET", target.URL, nil)
	response, err := c.Transport(nil).RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if c.Diagnostics().Spans.Accepted != 0 {
		t.Error("client span ended before response body")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Diagnostics().Spans.Accepted != 1 {
		t.Fatal("body close did not end client span")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
func TestDefaultDiagnosticsBypassApplicationLogBridge(t *testing.T) {
	_, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	previous := log.Writer()
	defer log.SetOutput(previous)
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		c.Log(context.Background(), agentrail.StructuredLog{EventName: "diagnostic-bridge"})
		return len(p), nil
	}))
	c.Identify(context.Background(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: "synthetic-user"})
	if c.Diagnostics().Logs.Accepted != 0 {
		t.Fatal("fallback diagnostics reentered application log bridge")
	}
}

func TestUpgradedBodyRetainsWriteAndReadErrors(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: synthetic\r\n\r\n")
		buf.Flush()
		var p [1]byte
		if _, err := io.ReadFull(conn, p[:]); err == nil {
			conn.Write(p[:])
		}
	}))
	defer target.Close()
	ctx, end := c.StartMilestone(context.Background(), "upgrade", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "upgrade"})
	q, _ := http.NewRequestWithContext(ctx, "GET", target.URL, nil)
	q.Header.Set("Connection", "Upgrade")
	q.Header.Set("Upgrade", "synthetic")
	response, err := c.Transport(nil).RoundTrip(q)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		response.Body.Close()
		t.Fatal("lost upgraded response writer")
	}
	if _, err := body.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var p [1]byte
	if _, err := io.ReadFull(body, p[:]); err != nil || p[0] != 'x' {
		t.Fatalf("upgrade changed: %q %v", p, err)
	}
	body.Close()
	end(nil)
	c.Flush(context.Background())
	r.mu.Lock()
	if len(spans(r)) != 2 {
		t.Error("missing upgrade spans")
	}
	r.mu.Unlock()
}

func TestResponseReadFailureMarksClientSpan(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nx")
		buf.Flush()
	}))
	defer target.Close()
	ctx, end := c.StartMilestone(context.Background(), "read", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "read"})
	q, _ := http.NewRequestWithContext(ctx, "GET", target.URL, nil)
	response, err := c.Transport(nil).RoundTrip(q)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(response.Body)
	if readErr != io.ErrUnexpectedEOF {
		t.Fatalf("changed read error %v", readErr)
	}
	response.Body.Close()
	end(nil)
	c.Flush(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	found := false
	for _, sp := range spans(r) {
		if sp.Kind == 3 {
			found = true
			if sp.Status.Code != 2 || attr(sp.Attributes, "error.type").GetStringValue() != "TransportError" {
				t.Fatal("lost response read failure")
			}
		}
	}
	if !found {
		t.Fatal("missing client span")
	}
}
