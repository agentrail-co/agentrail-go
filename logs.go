package agentrail

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	apilog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
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
	c.logger.Emit(ctx, r)
}
func (c *Client) copyLog(ctx context.Context, r *sdklog.Record) {
	fields := map[string]any{}
	if r.Body().Type() == attribute.STRING {
		fields["message"] = r.Body().AsString()
	}
	input := StructuredLog{SeverityNumber: int(r.Severity()), SeverityText: r.SeverityText(), EventName: r.EventName(), Fields: fields}
	r.WalkAttributes(func(a attribute.KeyValue) bool {
		key := string(a.Key)
		switch key {
		case "error.type", "exception.type":
			if a.Value.Type() == attribute.STRING {
				input.ErrorType = a.Value.AsString()
			}
		case "event.name":
			if a.Value.Type() == attribute.STRING {
				input.EventName = a.Value.AsString()
			}
		}
		switch a.Value.Type() {
		case attribute.STRING:
			fields[key] = a.Value.AsString()
		case attribute.BOOL:
			fields[key] = a.Value.AsBool()
		case attribute.INT64:
			fields[key] = a.Value.AsInt64()
		case attribute.FLOAT64:
			fields[key] = a.Value.AsFloat64()
		}
		return true
	})
	// The log record's exact correlation wins over any unrelated active span.
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: r.TraceID(), SpanID: r.SpanID(), TraceFlags: r.TraceFlags()})
	ctx = trace.ContextWithSpanContext(ctx, sc)
	c.emitLog(ctx, input, r.Timestamp(), r.ObservedTimestamp())
}
