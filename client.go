// Package agentrail records privacy-filtered backend evidence for Agentrail.
// It uses explicit request contexts and never installs global OTel providers.
package agentrail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/agentrail-co/agentrail-go/internal/delivery"
	"github.com/agentrail-co/agentrail-go/internal/handoff"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	apilog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type stateKey struct{ client *Client }
type markKey struct{ client *Client }

// spanKey holds our current span: the parent of the next span we start, and
// the span our logs are correlated to. It sits beside OpenTelemetry's current
// span, which belongs to the application (its own tracing, Sentry's or
// Datadog's): we never take it as a parent, clear it or replace it.
type spanKey struct{ client *Client }
type requestState struct {
	mu       sync.Mutex
	identity []attribute.KeyValue
	location []attribute.KeyValue
	verified bool
	links    []trace.Link
	// What a CLI's x-agentrail-cli header said of the request (cli.go), set
	// before the handler runs and never changed after. Only the request's
	// entry span, our one server span in it, carries it.
	cli []attribute.KeyValue
}

func (s *requestState) principal() []attribute.KeyValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]attribute.KeyValue(nil), s.identity...)
}

type mark struct {
	name        string
	details     MilestoneContext
	identity    []attribute.KeyValue
	performance []attribute.KeyValue
	mcp         *mcpMark
}
type spanMeta struct {
	state *requestState
	mark  *mark
}
type Client struct {
	options             Options
	resource            *resource.Resource
	provider            *sdktrace.TracerProvider
	tracer              trace.Tracer
	canonicalLogs       *sdklog.LoggerProvider
	logger              apilog.Logger
	spans               *delivery.Queue[sdktrace.ReadOnlySpan]
	payloadBytes        atomic.Int64
	logs                *delivery.Queue[sdklog.Record]
	metadata            sync.Map
	closed              atomic.Bool
	shutdownOnce        sync.Once
	shutdownDone        chan struct{}
	shutdownErr         error
	stopSpans, stopLogs func(context.Context) error
}
type Diagnostics struct {
	Spans delivery.Stats
	Logs  delivery.Stats
}

func (c *Client) Diagnostics() Diagnostics {
	return Diagnostics{Spans: c.spans.Diagnostics(), Logs: c.logs.Diagnostics()}
}
func (c *Client) state(ctx context.Context) *requestState {
	s, _ := ctx.Value(stateKey{c}).(*requestState)
	return s
}
func (c *Client) managed(ctx context.Context) bool { return !suppressed(ctx) && c.state(ctx) != nil }

// current is our span in ctx, or nil outside our spans.
func (c *Client) current(ctx context.Context) trace.Span {
	s, _ := ctx.Value(spanKey{c}).(trace.Span)
	return s
}

// parent is our current span's context in ctx; not valid outside our spans.
func (c *Client) parent(ctx context.Context) trace.SpanContext {
	if s := c.current(ctx); s != nil {
		return s.SpanContext()
	}
	return trace.SpanContext{}
}

// startSpan starts every span we record: a child of parent, or a new root
// when parent is not valid. Our tracer gets a context of its own, carrying the
// request state and mark the span processor reads at start. The context
// returned to the application is ctx with our span under our key only, so
// whatever OpenTelemetry span was current in ctx stays current.
func (c *Client) startSpan(ctx context.Context, parent trace.SpanContext, name string, m *mark, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	start := context.WithValue(ctx, markKey{c}, m)
	// Either way no application span is left current in our tracer's context:
	// the OTel SDK counts a child on whatever SDK span is current there, even
	// for a new root.
	if parent.IsValid() {
		start = trace.ContextWithSpanContext(start, parent)
	} else {
		start = trace.ContextWithSpanContext(start, trace.SpanContext{})
	}
	_, span := c.tracer.Start(start, name, opts...)
	return context.WithValue(ctx, spanKey{c}, span), span
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (c *Client) report(ctx context.Context, code, signal string, n int) {
	if suppressed(ctx) {
		return
	}
	c.options.report(suppress(ctx), code, signal, n)
}

// spanLimits are the SDK's defaults, given explicitly so that the
// application's OTEL_SPAN_* and OTEL_ATTRIBUTE_* limits never cut our spans.
var spanLimits = sdktrace.SpanLimits{
	AttributeValueLengthLimit:   sdktrace.DefaultAttributeValueLengthLimit,
	AttributeValueDepthLimit:    sdktrace.DefaultAttributeValueDepthLimit,
	AttributeCountLimit:         sdktrace.DefaultAttributeCountLimit,
	EventCountLimit:             sdktrace.DefaultEventCountLimit,
	LinkCountLimit:              sdktrace.DefaultLinkCountLimit,
	AttributePerEventCountLimit: sdktrace.DefaultAttributePerEventCountLimit,
	AttributePerLinkCountLimit:  sdktrace.DefaultAttributePerLinkCountLimit,
}

// New starts capture with tracer and logger providers of its own. It
// registers no OpenTelemetry global and replaces nothing the application set
// up, so it runs beside Sentry, Datadog or the application's own
// OpenTelemetry, before or after them.
func New(ctx context.Context, input Options) (*Client, error) {
	o, err := resolve(input)
	if err != nil {
		return nil, err
	}
	// The application's OTEL_* variables configure its own OpenTelemetry, so
	// every setting our exporters and providers would read from them is given
	// here: no vendor header, compression, certificate, timeout or limit of
	// theirs reaches our collector. A nil TLS configuration is the standard
	// one, and still counts as given, so OTEL_EXPORTER_OTLP_CERTIFICATE and
	// the client certificate and key variables are never read.
	headers := map[string]string{"Authorization": "Bearer " + o.Collector.Secret}
	tx, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(o.Collector.Endpoint+"/v1/traces"), otlptracehttp.WithHeaders(headers), otlptracehttp.WithTimeout(o.Queue.ExportTimeout), otlptracehttp.WithCompression(otlptracehttp.NoCompression), otlptracehttp.WithTLSClientConfig(nil))
	if err != nil {
		return nil, err
	}
	lx, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(o.Collector.Endpoint+"/v1/logs"), otlploghttp.WithHeaders(headers), otlploghttp.WithTimeout(o.Queue.ExportTimeout), otlploghttp.WithCompression(otlploghttp.NoCompression), otlploghttp.WithTLSClientConfig(nil))
	if err != nil {
		return nil, errors.Join(err, tx.Shutdown(ctx))
	}
	c := &Client{options: o, resource: resourceOf(o), shutdownDone: make(chan struct{})}
	cfg := delivery.Config{MaxRecords: o.Queue.MaxRecords, BatchSize: o.Queue.BatchSize, ScheduledDelay: o.Queue.ScheduledDelay, ExportTimeout: o.Queue.ExportTimeout, FlushTimeout: o.Queue.FlushTimeout, ShutdownTimeout: o.Queue.ShutdownTimeout}
	spanCfg := cfg
	spanCfg.MaxBatchBytes = batchBytes
	c.spans = delivery.NewWithHooks(spanCfg, func(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
		return tx.ExportSpans(suppress(ctx), spans)
	}, func(i delivery.Issue) { c.report(context.Background(), i.Code, "traces", i.Count) }, delivery.Hooks[sdktrace.ReadOnlySpan]{
		Size: func(s sdktrace.ReadOnlySpan) int { return s.(sanitizedSpan).size },
		Done: func(s sdktrace.ReadOnlySpan) { c.payloadBytes.Add(-int64(s.(sanitizedSpan).payload)) },
	})
	c.logs = delivery.New(cfg, func(ctx context.Context, records []sdklog.Record) error { return lx.Export(suppress(ctx), records) }, func(i delivery.Issue) { c.report(context.Background(), i.Code, "logs", i.Count) })
	sp := &spanProcessor{client: c, shutdownExporter: tx.Shutdown}
	qp := &logQueueProcessor{client: c, shutdownExporter: lx.Shutdown}
	c.provider = sdktrace.NewTracerProvider(sdktrace.WithResource(c.resource), sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithRawSpanLimits(spanLimits), sdktrace.WithSpanProcessor(sp))
	c.tracer = c.provider.Tracer("agentrail-go")
	c.canonicalLogs = sdklog.NewLoggerProvider(sdklog.WithResource(c.resource), sdklog.WithAttributeCountLimit(sdktrace.DefaultAttributeCountLimit), sdklog.WithAttributeValueLengthLimit(sdktrace.DefaultAttributeValueLengthLimit), sdklog.WithProcessor(qp))
	c.logger = c.canonicalLogs.Logger("agentrail-go")
	c.stopSpans = sp.Shutdown
	c.stopLogs = qp.Shutdown
	return c, nil
}

// Flush waits for delivery attempts, not guaranteed delivery. Inspect Diagnostics
// and OnError for records lost to refusal, timeout or queue overflow.
func (c *Client) Flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.options.Queue.FlushTimeout)
	defer cancel()
	return together(ctx, c.spans.Flush, c.logs.Flush)
}
func together(ctx context.Context, one, two func(context.Context) error) error {
	results := make(chan error, 2)
	go func() { results <- one(ctx) }()
	go func() { results <- two(ctx) }()
	return errors.Join(<-results, <-results)
}
func (c *Client) Shutdown(ctx context.Context) error {
	c.shutdownOnce.Do(func() {
		c.closed.Store(true)
		go func() {
			defer close(c.shutdownDone)
			drain, cancel := context.WithTimeout(ctx, c.options.Queue.ShutdownTimeout)
			defer cancel()
			c.shutdownErr = together(drain, c.stopSpans, c.stopLogs)
			c.shutdownErr = errors.Join(c.shutdownErr, c.canonicalLogs.Shutdown(drain), c.provider.Shutdown(drain))
		}()
	})
	select {
	case <-c.shutdownDone:
		return c.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func resourceOf(o Options) *resource.Resource {
	instance := o.ServiceInstanceID
	if instance == "" {
		instance = randomID()
	}
	return resource.NewSchemaless(attribute.String("service.name", o.ServiceName), attribute.String("service.instance.id", instance), attribute.String("agentrail.component.name", "agentrail-go"), attribute.String("agentrail.component.version", Version), attribute.String("agentrail.hash_key_id", handoff.KeyID(o.HashKey)), attribute.String("agentrail.capture.surfaces", "http,mcp,auth,custom"))
}
