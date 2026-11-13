package agentrail

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/agentrail-co/agentrail-go/internal/handoff"
	"github.com/agentrail-co/agentrail-go/internal/mcpspan"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Embedding the interface supplies its private marker method. Every readable
// method is implemented from a sanitized snapshot; no original span is retained.
type sanitizedSpan struct {
	sdktrace.ReadOnlySpan
	name       string
	attrs      []attribute.KeyValue
	links      []sdktrace.Link
	res        *resource.Resource
	sc, parent trace.SpanContext
	kind       trace.SpanKind
	start, end time.Time
	status     sdktrace.Status
	// size estimates the encoded span; payload is its MCP payload text length.
	size, payload int
}

func (s sanitizedSpan) Name() string                     { return s.name }
func (s sanitizedSpan) Attributes() []attribute.KeyValue { return s.attrs }
func (s sanitizedSpan) Events() []sdktrace.Event         { return nil }
func (s sanitizedSpan) Links() []sdktrace.Link           { return s.links }
func (s sanitizedSpan) Resource() *resource.Resource     { return s.res }
func (s sanitizedSpan) Status() sdktrace.Status          { return s.status }
func (s sanitizedSpan) SpanContext() trace.SpanContext   { return s.sc }
func (s sanitizedSpan) Parent() trace.SpanContext        { return s.parent }
func (s sanitizedSpan) SpanKind() trace.SpanKind         { return s.kind }
func (s sanitizedSpan) StartTime() time.Time             { return s.start }
func (s sanitizedSpan) EndTime() time.Time               { return s.end }
func (s sanitizedSpan) DroppedAttributes() int           { return 0 }
func (s sanitizedSpan) DroppedEvents() int               { return 0 }
func (s sanitizedSpan) DroppedLinks() int                { return 0 }
func (s sanitizedSpan) ChildSpanCount() int              { return 0 }
func (s sanitizedSpan) InstrumentationScope() instrumentation.Scope {
	return instrumentation.Scope{Name: "agentrail-go", Version: Version}
}
func (s sanitizedSpan) InstrumentationLibrary() instrumentation.Library {
	return s.InstrumentationScope()
}
func cleanSC(sc trace.SpanContext) trace.SpanContext { return sc.WithTraceState(trace.TraceState{}) }
func sanitizedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	u.User = nil
	u.Fragment = ""
	u.RawFragment = ""
	q := url.Values{}
	for _, v := range u.Query()["h"] {
		if handoff.ValidMarker(v) {
			q.Add("h", v)
		}
	}
	u.RawQuery = q.Encode()
	u.ForceQuery = false
	s := u.String()
	if !validText(s, 4096) {
		return ""
	}
	return s
}
func (c *Client) copySpan(s sdktrace.ReadOnlySpan, m spanMeta) sdktrace.ReadOnlySpan {
	original := make(map[string]attribute.Value)
	for _, a := range s.Attributes() {
		original[string(a.Key)] = a.Value
	}
	attrs := []attribute.KeyValue{}
	pairs := [][2]string{{"http.request.method", "http.method"}, {"http.route", "http.route"}, {"http.response.status_code", "http.status_code"}, {"user_agent.original", "http.user_agent"}, {"error.type", "exception.type"}}
	for _, p := range pairs {
		v, ok := original[p[0]]
		if !ok {
			v, ok = original[p[1]]
		}
		if !ok {
			continue
		}
		switch v.Type() {
		case attribute.STRING:
			attrs = append(attrs, attribute.String(p[0], bounded(v.AsString(), 4096)))
		case attribute.INT64:
			attrs = append(attrs, attribute.Int64(p[0], v.AsInt64()))
		}
	}
	if _, ok := original["error.type"]; !ok {
		for _, e := range s.Events() {
			if e.Name == "exception" {
				for _, a := range e.Attributes {
					if a.Key == "exception.type" && a.Value.Type() == attribute.STRING {
						attrs = append(attrs, attribute.String("error.type", bounded(a.Value.AsString(), 4096)))
					}
				}
			}
		}
	}
	raw := original["url.full"].AsString()
	if raw == "" {
		raw = original["http.url"].AsString()
	}
	if raw != "" {
		if u := sanitizedURL(raw); u != "" {
			attrs = append(attrs, attribute.String("url.full", u))
		}
	}
	// A handler span inside its transport span is no step and adds no identity.
	step := m.mark != nil && (m.mark.mcp == nil || m.mark.mcp.step)
	entry := step || s.SpanKind() == trace.SpanKindServer || s.SpanKind() == trace.SpanKindConsumer
	if entry {
		identity := m.state.principal()
		if m.mark != nil && m.mark.identity != nil {
			identity = m.mark.identity
		}
		attrs = append(attrs, identity...)
		attrs = append(attrs, m.state.location...)
	}
	if s.SpanKind() == trace.SpanKindServer {
		attrs = append(attrs, attribute.Bool("agentrail.ingress.verified", m.state.verified))
	}
	name := "backend.operation"
	if v := original["http.request.method"]; v.Type() == attribute.STRING {
		name = "HTTP " + v.AsString()
	} else if v := original["http.method"]; v.Type() == attribute.STRING {
		name = "HTTP " + v.AsString()
	}
	payload := 0
	if m.mark != nil {
		d := m.mark.details
		name = m.mark.name
		if step {
			attrs = append(attrs, attribute.Bool("agentrail.step", true), attribute.String("agentrail.step.surface", string(d.Surface)), attribute.String("agentrail.step.kind", d.Kind))
			attrs = append(attrs, m.mark.performance...)
		}
		if d.MCP != nil {
			// Resources have no standard name attribute: their URI is omitted.
			toolName, promptName := "", ""
			switch d.MCP.Method {
			case "tools/call":
				toolName = d.MCP.Name
			case "prompts/get":
				promptName = d.MCP.Name
			}
			for _, v := range []struct {
				k, v string
				max  int
			}{{"rpc.method", d.MCP.Method, 4096}, {"mcp.method.name", d.MCP.Method, 128}, {"gen_ai.tool.name", toolName, 256}, {"gen_ai.prompt.name", promptName, 256}} {
				if v.v != "" {
					attrs = append(attrs, attribute.String(v.k, bounded(v.v, v.max)))
				}
			}
			if m.mark.mcp == nil {
				attrs = append(attrs, mcpMethodAttr(d.MCP.Method)...)
			}
			attrs = append(attrs, c.mcpMetaAttrs(mcpspan.Meta{Client: d.MCP.Client, ClientVersion: d.MCP.ClientVersion, Protocol: d.MCP.Protocol})...)
		}
		if m.mark.mcp != nil {
			var extra []attribute.KeyValue
			extra, payload = m.mark.mcp.snapshot()
			attrs = append(attrs, extra...)
		}
	}
	links := []sdktrace.Link{}
	for _, l := range s.Links() {
		links = append(links, sdktrace.Link{SpanContext: cleanSC(l.SpanContext)})
	}
	if s.SpanKind() == trace.SpanKindServer {
		for _, l := range m.state.links {
			links = append(links, sdktrace.Link{SpanContext: cleanSC(l.SpanContext)})
		}
	}
	name = bounded(name, 4096)
	return sanitizedSpan{sc: cleanSC(s.SpanContext()), parent: cleanSC(s.Parent()), kind: s.SpanKind(), start: s.StartTime(), end: s.EndTime(), status: sdktrace.Status{Code: s.Status().Code}, name: name, attrs: attrs, links: links, res: c.resource, size: encodedSize(name, attrs, links), payload: payload}
}

// encodedSize overestimates a span's OTLP protobuf size, for byte-sized batches.
func encodedSize(name string, attrs []attribute.KeyValue, links []sdktrace.Link) int {
	n := 128 + len(name) + 48*len(links)
	for _, a := range attrs {
		n += 16 + len(a.Key)
		if a.Value.Type() == attribute.STRING {
			n += len(a.Value.AsString())
		} else {
			n += 10
		}
	}
	return n
}
func marshal(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if e.Encode(v) != nil {
		return nil
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
func scalar(v any) (any, bool) {
	switch x := v.(type) {
	case nil, bool:
		return x, true
	case string:
		return bounded(x, 4096), true
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return x, true
	case float32:
		if math.IsInf(float64(x), 0) || math.IsNaN(float64(x)) {
			return nil, false
		}
		return x, true
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil, false
		}
		return x, true
	}
	return nil, false
}
func selectedBody(fields map[string]any, selected []string) string {
	parts := []string{}
	used := 2
	seen := map[string]bool{}
	for _, key := range selected {
		v, ok := fields[key]
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		value, ok := scalar(v)
		if !ok {
			continue
		}
		k := marshal(bounded(key, 4096))
		if k == nil {
			continue
		}
		prefix := len(parts) > 0
		overhead := textLen(string(k)) + 1
		if prefix {
			overhead++
		}
		budget := 4096 - used - overhead
		if budget < 1 {
			continue
		}
		encoded := marshal(value)
		if text, ok := value.(string); ok && textLen(string(encoded)) > budget {
			runes := []rune(text)
			lo, hi := 0, len(runes)
			for lo < hi {
				mid := (lo + hi + 1) / 2
				if textLen(string(marshal(string(runes[:mid])))) <= budget {
					lo = mid
				} else {
					hi = mid - 1
				}
			}
			encoded = marshal(string(runes[:lo]))
		}
		if encoded == nil || textLen(string(encoded)) > budget {
			continue
		}
		part := string(k) + ":" + string(encoded)
		parts = append(parts, part)
		used += overhead + textLen(string(encoded))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}
