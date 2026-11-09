package agentrail

import (
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/agentrail-co/agentrail-go/internal/handoff"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// A private standard network transport is immune to process-wide tracing wrappers.
var defaultNetworkTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
}

type transport struct {
	client *Client
	base   *http.Transport
}

// Transport instruments a network transport. A concrete *http.Transport keeps
// final propagation adjacent to the network: an arbitrary nested tracing
// RoundTripper could otherwise re-inject forbidden headers after signing.
// A nil base uses a private standard network transport. Client redirects pass through this
// wrapper again, so each actual destination is admitted independently.
func (c *Client) Transport(base *http.Transport) http.RoundTripper {
	if base == nil {
		base = defaultNetworkTransport
	}
	return &transport{client: c, base: base}
}
func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := t.client
	if !c.managed(r.Context()) || c.closed.Load() {
		return t.base.RoundTrip(r)
	}
	if c.collectorRequest(r) {
		copy := r.Clone(r.Context())
		clearHeaders(copy.Header)
		return t.base.RoundTrip(copy)
	}
	ctx, span := c.tp.Tracer("agentrail-go").Start(r.Context(), "HTTP "+r.Method, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("http.request.method", r.Method), attribute.String("url.full", r.URL.String())))
	copy := r.Clone(ctx)
	clearHeaders(copy.Header)
	for _, allowed := range c.options.OutboundOrigins {
		if origin(copy.URL) == allowed {
			sc := span.SpanContext()
			tp := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + sc.TraceFlags().String()
			method := copy.Method
			if method == "" {
				method = "GET"
			}
			token, err := handoff.Sign(c.options.HashKey, tp, method, handoff.Target(copy.URL), time.Now().UnixMilli())
			if err != nil {
				c.report(ctx, "handoff_refused", "context", 1)
			} else {
				copy.Header.Set("traceparent", tp)
				copy.Header.Set(handoffHeader, token)
			}
			break
		}
	}
	response, err := t.base.RoundTrip(copy)
	if err != nil {
		span.SetStatus(codes.Error, "")
		span.SetAttributes(attribute.String("error.type", "TransportError"))
		span.End()
		return response, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
	if response.StatusCode >= 400 {
		span.SetStatus(codes.Error, "")
	}
	if response.Body == nil || response.Body == http.NoBody {
		span.End()
	} else {
		body := &responseBody{ReadCloser: response.Body, span: span}
		if writer, ok := response.Body.(io.Writer); ok {
			response.Body = &upgradeBody{responseBody: body, writer: writer}
		} else {
			response.Body = body
		}
	}
	return response, nil
}

type responseBody struct {
	io.ReadCloser
	span trace.Span
	once sync.Once
}

func (b *responseBody) finish(err error) {
	b.once.Do(func() {
		if err != nil && err != io.EOF {
			b.span.SetStatus(codes.Error, "")
			b.span.SetAttributes(attribute.String("error.type", "TransportError"))
		}
		b.span.End()
	})
}
func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish(err)
	}
	return n, err
}
func (b *responseBody) Close() error { err := b.ReadCloser.Close(); b.finish(err); return err }

type upgradeBody struct {
	*responseBody
	writer io.Writer
}

func (b *upgradeBody) Write(p []byte) (int, error) {
	n, err := b.writer.Write(p)
	if err != nil {
		b.finish(err)
	}
	return n, err
}
