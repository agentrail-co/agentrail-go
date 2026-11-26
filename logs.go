package agentrail

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	apilog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

type StructuredLog struct {
	SeverityNumber                     int
	SeverityText, ErrorType, EventName string
	Fields                             map[string]any
	Timestamp, ObservedTimestamp       time.Time
}

// Log records structured fields only. Message text requires Logs.IncludeFields.
func (c *Client) Log(ctx context.Context, input StructuredLog) {
	if suppressed(ctx) {
		return
	}
	if c.closed.Load() {
		c.report(ctx, "closed", "logs", 1)
		return
	}
	if c.options.Logs.ExcludeUncorrelated && !c.managed(ctx) {
		return
	}
	stamp := input.Timestamp
	if stamp.IsZero() {
		stamp = time.Now()
	}
	observed := input.ObservedTimestamp
	if observed.IsZero() {
		observed = time.Now()
	}
	c.emitLog(ctx, input, stamp, observed)
}
func (c *Client) emitLog(ctx context.Context, input StructuredLog, stamp, observed time.Time) {
	var r apilog.Record
	r.SetTimestamp(stamp)
	r.SetObservedTimestamp(observed)
	if input.SeverityNumber >= 0 && input.SeverityNumber <= 24 {
		r.SetSeverity(apilog.Severity(input.SeverityNumber))
	}
	r.SetSeverityText(bounded(input.SeverityText, 64))
	attrs := []attribute.KeyValue{attribute.String("agentrail.log.id", randomID())}
	if input.ErrorType != "" {
		attrs = append(attrs, attribute.String("error.type", bounded(input.ErrorType, 4096)))
	}
	if input.EventName != "" {
		r.SetEventName(bounded(input.EventName, 256))
	}
	if body := selectedBody(input.Fields, c.options.Logs.IncludeFields); body != "" {
		r.SetBody(attribute.StringValue(body))
		attrs = append(attrs, attribute.Bool("agentrail.log.body_allowed", true))
	}
	r.AddAttributes(attrs...)
	// Correlated to our current span only. The application's own current span
	// names a trace Agentrail never receives, so a log outside ours has none.
	c.logger.Emit(trace.ContextWithSpanContext(ctx, c.parent(ctx)), r)
}
