package agentrail

import (
	"context"
	"errors"
	"sync"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// spanProcessor runs only in the Client's own tracer provider, so every span
// it sees is one of ours; each is copied, sanitized, into the span queue.
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
	c := p.client
	span := c.copySpan(s, v.(spanMeta)).(sanitizedSpan)
	if span.payload > 0 && c.payloadBytes.Add(int64(span.payload)) > payloadBudgetBytes {
		// Past the budget the span is kept and only its payload is dropped.
		c.payloadBytes.Add(-int64(span.payload))
		span = span.withoutPayload()
		c.report(context.Background(), "payload_budget", "traces", 1)
	}
	if !c.spans.Add(span) {
		c.payloadBytes.Add(-int64(span.payload))
	}
}
func (p *spanProcessor) ForceFlush(ctx context.Context) error { return p.client.spans.Flush(ctx) }
func (p *spanProcessor) Shutdown(ctx context.Context) error {
	p.once.Do(func() { p.err = errors.Join(p.client.spans.Shutdown(ctx), p.shutdownExporter(ctx)) })
	return p.err
}

// logQueueProcessor runs only in the Client's own logger provider, which
// only Client.Log writes to.
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
