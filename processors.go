package agentrail

import (
	"context"
	"errors"
	"sync"

	"github.com/agentrail-co/agentrail-go/internal/delivery"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Processors must be constructed before the customer's provider. Bind that
// provider once, then use the returned Client's HTTP wrappers and operations.
type Processors struct {
	SpanProcessor sdktrace.SpanProcessor
	LogProcessor  sdklog.Processor
	Propagator    propagation.TextMapPropagator
	client        *Client
	bindMu        sync.Mutex
}

func (p *Processors) Bind(tp trace.TracerProvider) (*Client, error) {
	p.bindMu.Lock()
	defer p.bindMu.Unlock()
	if tp == nil {
		return nil, &ConfigurationError{Problem: "a tracer provider is required"}
	}
	if p.client.bound.Load() {
		return nil, &ConfigurationError{Problem: "processors already bound"}
	}
	if p.client.closed.Load() {
		return nil, ErrClosed
	}
	p.client.tp = tp
	p.client.bound.Store(true)
	return p.client, nil
}
func (p *Processors) Sampler(base sdktrace.Sampler) sdktrace.Sampler {
	return managedSampler{client: p.client, base: base}
}

type managedSampler struct {
	client *Client
	base   sdktrace.Sampler
}

func (s managedSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	r := s.base.ShouldSample(p)
	if r.Decision == sdktrace.Drop && s.client.managed(p.ParentContext) {
		r.Decision = sdktrace.RecordOnly
	}
	return r
}
func (s managedSampler) Description() string { return "Agentrail(" + s.base.Description() + ")" }
func NewProcessors(ctx context.Context, input Options) (*Processors, error) {
	o, err := resolve(input)
	if err != nil {
		return nil, err
	}
	tx, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(o.Collector.Endpoint+"/v1/traces"), otlptracehttp.WithHeaders(map[string]string{"Authorization": "Bearer " + o.Collector.Secret}), otlptracehttp.WithTimeout(o.Queue.ExportTimeout))
	if err != nil {
		return nil, err
	}
	lx, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(o.Collector.Endpoint+"/v1/logs"), otlploghttp.WithHeaders(map[string]string{"Authorization": "Bearer " + o.Collector.Secret}), otlploghttp.WithTimeout(o.Queue.ExportTimeout))
	if err != nil {
		return nil, errors.Join(err, tx.Shutdown(ctx))
	}
	c := &Client{options: o, resource: resourceOf(o), shutdownDone: make(chan struct{})}
	cfg := delivery.Config{MaxRecords: o.Queue.MaxRecords, BatchSize: o.Queue.BatchSize, ScheduledDelay: o.Queue.ScheduledDelay, ExportTimeout: o.Queue.ExportTimeout, FlushTimeout: o.Queue.FlushTimeout, ShutdownTimeout: o.Queue.ShutdownTimeout}
	c.spans = delivery.New(cfg, func(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
		return tx.ExportSpans(suppress(ctx), spans)
	}, func(i delivery.Issue) { c.report(context.Background(), i.Code, "traces", i.Count) })
	c.logs = delivery.New(cfg, func(ctx context.Context, records []sdklog.Record) error { return lx.Export(suppress(ctx), records) }, func(i delivery.Issue) { c.report(context.Background(), i.Code, "logs", i.Count) })
	sp := &spanProcessor{client: c, shutdownExporter: tx.Shutdown}
	lp := &logProcessor{client: c}
	qp := &logQueueProcessor{client: c, shutdownExporter: lx.Shutdown}
	c.canonicalLogs = sdklog.NewLoggerProvider(sdklog.WithResource(c.resource), sdklog.WithProcessor(qp))
	c.logger = c.canonicalLogs.Logger("agentrail-go")
	// Only the private logger sees the sanitized resource. The customer's
	// mutable Record and resource are never rewritten for other processors.
	c.stopSpans = sp.Shutdown
	c.stopLogs = qp.Shutdown
	return &Processors{SpanProcessor: sp, LogProcessor: lp, Propagator: requestPropagator{client: c, base: o.Propagator}, client: c}, nil
}

type spanProcessor struct {
	client           *Client
	shutdownExporter func(context.Context) error
	once             sync.Once
	err              error
}

func (p *spanProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	if !p.client.managed(ctx) || p.client.closed.Load() {
		return
	}
	m, _ := ctx.Value(markKey{p.client}).(*mark)
	p.client.metadata.Store(s.SpanContext().SpanID(), spanMeta{p.client.state(ctx), m})
}
func (p *spanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	v, ok := p.client.metadata.LoadAndDelete(s.SpanContext().SpanID())
	if !ok {
		return
	}
	p.client.spans.Add(p.client.copySpan(s, v.(spanMeta)))
}
func (p *spanProcessor) ForceFlush(ctx context.Context) error { return p.client.spans.Flush(ctx) }
func (p *spanProcessor) Shutdown(ctx context.Context) error {
	p.once.Do(func() { p.err = errors.Join(p.client.spans.Shutdown(ctx), p.shutdownExporter(ctx)) })
	return p.err
}

type logProcessor struct{ client *Client }

func (p *logProcessor) Enabled(ctx context.Context, _ sdklog.EnabledParameters) bool {
	return !suppressed(ctx) && !p.client.closed.Load() && (!p.client.options.Logs.ExcludeUncorrelated || p.client.managed(ctx))
}
func (p *logProcessor) OnEmit(ctx context.Context, r *sdklog.Record) error {
	if !p.Enabled(ctx, sdklog.EnabledParameters{}) {
		return nil
	}
	p.client.copyLog(ctx, r)
	return nil
}
func (p *logProcessor) ForceFlush(ctx context.Context) error { return p.client.logs.Flush(ctx) }
func (p *logProcessor) Shutdown(ctx context.Context) error   { return p.client.stopLogs(ctx) }

type logQueueProcessor struct {
	client           *Client
	shutdownExporter func(context.Context) error
	once             sync.Once
	err              error
}

func (p *logQueueProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (p *logQueueProcessor) OnEmit(_ context.Context, r *sdklog.Record) error {
	p.client.logs.Add(r.Clone())
	return nil
}
func (p *logQueueProcessor) ForceFlush(ctx context.Context) error { return p.client.logs.Flush(ctx) }
func (p *logQueueProcessor) Shutdown(ctx context.Context) error {
	p.once.Do(func() { p.err = errors.Join(p.client.logs.Shutdown(ctx), p.shutdownExporter(ctx)) })
	return p.err
}

// requestPropagator never accepts a public trace header as authenticated
// ancestry. Method/target verification belongs to the incoming HTTP wrapper.
type requestPropagator struct {
	client *Client
	base   propagation.TextMapPropagator
}

func (p requestPropagator) Inject(ctx context.Context, c propagation.TextMapCarrier) {
	if p.client.managed(ctx) || suppressed(ctx) {
		return
	}
	if p.base != nil {
		p.base.Inject(ctx, c)
	}
}
func (p requestPropagator) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	if p.client.managed(ctx) {
		return ctx
	}
	if p.base != nil {
		return p.base.Extract(ctx, c)
	}
	return ctx
}
func (p requestPropagator) Fields() []string {
	out := []string{"traceparent", "tracestate", "baggage", "sentry-trace", handoffHeader}
	if p.base != nil {
		out = append(out, p.base.Fields()...)
	}
	return out
}
