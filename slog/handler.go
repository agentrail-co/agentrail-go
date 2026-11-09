// Package slog adds opt-in Agentrail capture to an application's log/slog handler.
// Application output is still handled by the supplied handler. Message text and
// arbitrary scalar fields reach Agentrail only when selected in
// Options.Logs.IncludeFields. LogValuers are evaluated only by the application
// handler and are not captured.
package slog

import (
	"context"
	"log/slog"
	"reflect"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
)

type field struct {
	key   string
	value any
}
type handler struct {
	client *agentrail.Client
	next   slog.Handler
	groups []string
	fields []field
}

// NewHandler composes capture with next without changing its level filtering,
// attributes, grouping, output, or returned errors. Supply a non-nil client and
// handler. Use event.name for a stable event label and err/error attributes for
// an error's type; error messages are never converted into captured fields.
func NewHandler(client *agentrail.Client, next slog.Handler) slog.Handler {
	if client == nil || next == nil {
		panic("agentrail/slog: client and next handler are required")
	}
	return &handler{client: client, next: next}
}
func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func (h *handler) Handle(ctx context.Context, record slog.Record) error {
	fields := make(map[string]any, len(h.fields)+record.NumAttrs()+1)
	fields["message"] = record.Message
	for _, f := range h.fields {
		fields[f.key] = f.value
	}
	record.Attrs(func(a slog.Attr) bool { appendAttr(fields, h.groups, a); return true })
	input := agentrail.StructuredLog{SeverityNumber: severity(record.Level), SeverityText: record.Level.String(), Fields: fields, Timestamp: record.Time}
	if name, ok := fields["event.name"].(string); ok {
		input.EventName = name
	}
	if kind, ok := fields["error.type"].(string); ok {
		input.ErrorType = kind
	}
	h.client.Log(ctx, input)
	return h.next.Handle(ctx, record)
}
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.fields = append([]field(nil), h.fields...)
	for _, a := range attrs {
		values := map[string]any{}
		appendAttr(values, h.groups, a)
		for k, v := range values {
			next.fields = append(next.fields, field{k, v})
		}
	}
	next.next = h.next.WithAttrs(attrs)
	return &next
}
func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.next = h.next.WithGroup(name)
	next.groups = append(append([]string(nil), h.groups...), name)
	return &next
}
func severity(level slog.Level) int {
	// Standard slog levels are four apart, as are the corresponding OTel levels.
	n := int(level) + 9
	if n < 1 {
		return 1
	}
	if n > 24 {
		return 24
	}
	return n
}
func key(groups []string, name string) string {
	for i := len(groups) - 1; i >= 0; i-- {
		name = groups[i] + "." + name
	}
	return name
}
func appendAttr(fields map[string]any, groups []string, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			groups = append(append([]string(nil), groups...), a.Key)
		}
		for _, child := range a.Value.Group() {
			appendAttr(fields, groups, child)
		}
		return
	}
	name := key(groups, a.Key)
	switch a.Value.Kind() {
	case slog.KindString:
		fields[name] = a.Value.String()
	case slog.KindBool:
		fields[name] = a.Value.Bool()
	case slog.KindInt64:
		fields[name] = a.Value.Int64()
	case slog.KindUint64:
		fields[name] = a.Value.Uint64()
	case slog.KindFloat64:
		fields[name] = a.Value.Float64()
	case slog.KindDuration:
		fields[name] = a.Value.Duration().String()
	case slog.KindTime:
		fields[name] = a.Value.Time().Format(time.RFC3339Nano)
	case slog.KindAny:
		value := a.Value.Any()
		if err, ok := value.(error); ok {
			if name == "err" || name == "error" {
				fields["error.type"] = reflect.TypeOf(err).String()
			}
			return
		}
		// Do not stringify objects: Stringer and error implementations may expose
		// arbitrary customer content. Client.Log selects and sanitizes scalars.
		switch value.(type) {
		case nil, bool, string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			fields[name] = value
		}
	}
}
