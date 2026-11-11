package agentrail

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentrail-co/agentrail-go/internal/handoff"
	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const handoffHeader = "x-agentrail-trace-handoff"

var ownedHeaders = []string{"traceparent", "tracestate", "baggage", "sentry-trace", handoffHeader}

func singleHeader(h http.Header, key string) string {
	count := 0
	value := ""
	for k, vs := range h {
		if strings.EqualFold(k, key) {
			count += len(vs)
			if len(vs) > 0 {
				value = vs[0]
			}
		}
	}
	if count != 1 {
		return ""
	}
	return value
}
func clearHeaders(h http.Header) {
	for key := range h {
		for _, name := range ownedHeaders {
			if strings.EqualFold(key, name) {
				delete(h, key)
				break
			}
		}
	}
}
func (c *Client) collectorPath(path string) bool {
	return path == c.options.endpointPath+"/v1/traces" || path == c.options.endpointPath+"/v1/logs"
}
func (c *Client) collectorRequest(r *http.Request) bool {
	return origin(r.URL) == c.options.endpointOrigin && c.collectorPath(r.URL.EscapedPath())
}

// Middleware replaces incoming HTTP tracing for the admitted scope. Install it
// once; do not also wrap the same request in otelhttp.NewHandler.
func (c *Client) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if suppressed(r.Context()) || c.closed.Load() || (c.options.IgnoreIncomingRequest != nil && c.options.IgnoreIncomingRequest(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if err := c.ready(); err != nil {
			c.report(r.Context(), "entry_not_recording", "traces", 1)
			next.ServeHTTP(w, r)
			return
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		parent := singleHeader(r.Header, "traceparent")
		token := singleHeader(r.Header, handoffHeader)
		target := r.RequestURI
		if target == "" {
			target = handoff.Target(r.URL)
		}
		sc, err := handoff.Verify(c.options.HashKey, token, parent, r.Method, target, time.Now().UnixMilli())
		state := &requestState{verified: err == nil}
		state.location = c.captureLocation(r)
		ctx := trace.ContextWithSpanContext(r.Context(), trace.SpanContext{})
		if err == nil {
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		} else {
			if external, ok := handoff.ParseTraceparent(parent); ok {
				state.links = []trace.Link{{SpanContext: cleanSC(external)}}
			}
			if !errors.Is(err, handoff.ErrMissing) {
				c.report(ctx, "handoff_refused", "context", 1)
			}
		}
		ctx = context.WithValue(ctx, stateKey{c}, state)
		attrs := []attribute.KeyValue{attribute.String("http.request.method", r.Method), attribute.String("url.full", scheme+"://"+r.Host+target)}
		if ua := r.UserAgent(); ua != "" {
			attrs = append(attrs, attribute.String("user_agent.original", ua))
		}
		ctx, span := c.tp.Tracer("agentrail-go").Start(ctx, "HTTP "+r.Method, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		if !span.IsRecording() {
			c.report(ctx, "entry_not_recording", "traces", 1)
		}
		request := r.WithContext(ctx)
		status := http.StatusOK
		wrote := false
		writer := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					if code >= 200 && !wrote {
						status = code
						wrote = true
					}
					next(code)
				}
			},
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(b []byte) (int, error) { wrote = true; return next(b) }
			},
			Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc { return func() { wrote = true; next() } },
			ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(r io.Reader) (int64, error) { wrote = true; return next(r) }
			},
		})
		defer func() {
			if p := recover(); p != nil {
				span.SetStatus(codes.Error, "")
				span.SetAttributes(attribute.String("error.type", "panic"))
				span.End()
				panic(p)
			}
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			if request.Pattern != "" {
				span.SetAttributes(attribute.String("http.route", request.Pattern))
			}
			if status >= 500 {
				span.SetStatus(codes.Error, "")
			}
			span.End()
		}()
		next.ServeHTTP(writer, request)
	})
}
