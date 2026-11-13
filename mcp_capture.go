package agentrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/agentrail-co/agentrail-go/internal/handoff"
	"github.com/agentrail-co/agentrail-go/internal/mcpspan"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The contract's otlp.limits; a test holds them to testdata/sdk-contract.json.
const (
	// batchBytes is exportBatchBytes: a span batch closes before its estimated
	// size passes it, under the collector's 16 MiB body limit.
	batchBytes = 15 << 20
	// recordPayloadBytes is mcpPayloadBytes: the most payload text one span
	// carries, both payloads together, so that a span always fits one export.
	recordPayloadBytes = 14 << 20
	// payloadBudgetBytes bounds payload text held per span queue, in flight included.
	payloadBudgetBytes = 64 << 20
)

const (
	keyMCPRequestPayload   = "agentrail.mcp.request.payload"
	keyMCPResponsePayload  = "agentrail.mcp.response.payload"
	keyMCPRequestTruncated = "agentrail.mcp.request.truncated"
	keyMCPResponseTrunc    = "agentrail.mcp.response.truncated"
)

// The collector's pattern for agentrail.mcp.method; a method it would refuse is left off.
var mcpMethod = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,128}$`)

func init() {
	mcpspan.Transport = func(client any, ctx context.Context, call mcpspan.Call) mcpspan.Span {
		return client.(*Client).mcpTransport(ctx, call)
	}
	mcpspan.Handler = func(client any, ctx context.Context, parent mcpspan.Span, active bool, method, name string, meta mcpspan.Meta, run func(context.Context) error) error {
		return client.(*Client).mcpHandler(ctx, parent, active, method, name, meta, run)
	}
}

// mcpMark carries an MCP span's attributes. They are kept here rather than on
// the OTel span, so other processors of a shared provider never see payloads.
type mcpMark struct {
	layer   string
	step    bool
	mu      sync.Mutex
	attrs   []attribute.KeyValue
	payload int
}

func (m *mcpMark) add(payload int, attrs ...attribute.KeyValue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attrs = append(m.attrs, attrs...)
	m.payload += payload
}
func (m *mcpMark) snapshot() ([]attribute.KeyValue, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]attribute.KeyValue{attribute.String("agentrail.mcp.layer", m.layer)}, m.attrs...), m.payload
}

func mcpKind(method string) string {
	if method == "tools/call" {
		return "tool.call"
	}
	return bounded("mcp."+method, 256)
}

// mcpSpanName is mcp.<method>, or mcp.<method>:<name>, in 256 UTF-16 units.
func mcpSpanName(method, name string) string {
	if name == "" {
		return bounded("mcp."+method, 256)
	}
	return bounded("mcp."+method+":"+name, 256)
}

// resourceName is a resource URI without its userinfo, query or fragment,
// where credentials and tokens travel. It cuts at the delimiters rather than
// parsing, so a URI net/url would refuse is still stripped.
func resourceName(uri string) string {
	if i := strings.IndexAny(uri, "?#"); i >= 0 {
		uri = uri[:i]
	}
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return uri
	}
	authority, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:]
	}
	return scheme + "://" + authority + path
}
func mcpMethodAttr(method string) []attribute.KeyValue {
	if !mcpMethod.MatchString(method) {
		return nil
	}
	return []attribute.KeyValue{attribute.String("agentrail.mcp.method", method)}
}
func (c *Client) mcpMetaAttrs(m mcpspan.Meta) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}
	for _, v := range []struct {
		k, v string
		max  int
	}{{"agentrail.mcp.client.name", m.Client, 128}, {"agentrail.mcp.client.version", m.ClientVersion, 128}, {"agentrail.mcp.protocol.version", m.Protocol, 64}} {
		if v.v != "" {
			attrs = append(attrs, attribute.String(v.k, bounded(v.v, v.max)))
		}
	}
	if m.SessionID != "" {
		attrs = append(attrs, attribute.String("agentrail.mcp.session.id_hash", handoff.Hash(c.options.HashKey, m.SessionID)))
	}
	return attrs
}

type noMCPSpan struct{}

func (noMCPSpan) End(mcpspan.Outcome)                       {}
func (noMCPSpan) Graft(ctx context.Context) context.Context { return ctx }

type transportSpan struct {
	client *Client
	ctx    context.Context
	span   trace.Span
	mark   *mcpMark
	call   mcpspan.Call
	once   sync.Once
}

func (c *Client) mcpTransport(ctx context.Context, call mcpspan.Call) mcpspan.Span {
	m := &mcpMark{layer: "transport", step: true}
	name := call.Target
	if call.Method == "resources/read" {
		name = resourceName(name)
	}
	ctx, span := c.start(ctx, mcpSpanName(call.Method, name), MilestoneContext{Surface: SurfaceMCP, Kind: mcpKind(call.Method), MCP: &MCPContext{Method: call.Method, Name: name}}, m)
	if span == nil {
		return noMCPSpan{}
	}
	if !c.options.MCP.CapturePayloads {
		call.Params = nil // only payloads read them; a pending call need not hold them
	}
	return &transportSpan{client: c, ctx: ctx, span: span, mark: m, call: call}
}
func (t *transportSpan) Graft(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, stateKey{t.client}, t.client.state(t.ctx))
	return trace.ContextWithSpan(ctx, t.span)
}
func (t *transportSpan) End(o mcpspan.Outcome) {
	t.once.Do(func() { t.end(o) })
}
func (t *transportSpan) end(o mcpspan.Outcome) {
	c := t.client
	attrs := mcpMethodAttr(t.call.Method)
	attrs = append(attrs, c.mcpMetaAttrs(o.Meta)...)
	if t.call.RequestBytes >= 0 {
		attrs = append(attrs, attribute.Int("agentrail.mcp.request.bytes", t.call.RequestBytes))
	}
	failed := o.Rejected
	if o.Rejected {
		attrs = append(attrs, attribute.String("error.type", "McpTransportRejected"))
	}
	if o.Responded {
		attrs = append(attrs, attribute.Int("agentrail.mcp.response.bytes", o.ResponseBytes))
		if o.ErrorCode != nil {
			failed = true
			attrs = append(attrs, attribute.String("error.type", "McpJsonRpcError"))
			// The collector stores the code as int4 and refuses an export with one outside it.
			if *o.ErrorCode >= math.MinInt32 && *o.ErrorCode <= math.MaxInt32 {
				attrs = append(attrs, attribute.Int64("agentrail.mcp.jsonrpc.error_code", *o.ErrorCode))
			}
		} else if o.IsError {
			failed = true
			attrs = append(attrs, attribute.Bool("agentrail.mcp.result.is_error", true), attribute.String("error.type", "tool_error"))
		}
	}
	payload := 0
	if c.options.MCP.CapturePayloads {
		var p []attribute.KeyValue
		p, payload = c.mcpPayload(t.ctx, MCPPayload{Method: t.call.Method, Name: t.call.Target, Request: t.call.Params, Response: o.Result}, o.Responded)
		attrs = append(attrs, p...)
	}
	t.mark.add(payload, attrs...)
	if failed {
		t.span.SetStatus(codes.Error, "")
	}
	t.span.End()
}

// mcpPayload applies the redact hook and returns the payload attributes and
// their text length. Nothing it does can fail the MCP call.
func (c *Client) mcpPayload(ctx context.Context, p MCPPayload, responded bool) ([]attribute.KeyValue, int) {
	p.Request = bytes.Clone(p.Request)
	p.Response = bytes.Clone(p.Response)
	if !responded {
		p.Response = nil
	}
	// resolve refuses payload capture without a hook.
	keep, panicked := false, true
	func() {
		defer func() {
			if panicked {
				recover()
			}
		}()
		p, keep = c.options.MCP.Redact(p)
		panicked = false
	}()
	if panicked {
		c.report(ctx, "payload_redact_failed", "traces", 1)
		return nil, 0
	}
	if !keep {
		return nil, 0
	}
	for _, text := range [][]byte{p.Request, p.Response} {
		if len(text) > 0 && (!utf8.Valid(text) || !json.Valid(text)) {
			c.report(ctx, "payload_redact_failed", "traces", 1)
			return nil, 0
		}
	}
	request, requestCut, response, responseCut := fitPayload(string(p.Request), string(p.Response), recordPayloadBytes)
	attrs := []attribute.KeyValue{}
	if request != "" {
		attrs = append(attrs, attribute.String(keyMCPRequestPayload, request))
	}
	if response != "" {
		attrs = append(attrs, attribute.String(keyMCPResponsePayload, response))
	}
	if requestCut {
		attrs = append(attrs, attribute.Bool(keyMCPRequestTruncated, true))
	}
	if responseCut {
		attrs = append(attrs, attribute.Bool(keyMCPResponseTrunc, true))
	}
	return attrs, len(request) + len(response)
}

// fitPayload cuts a request and response, together longer than limit bytes,
// to fit it as valid UTF-8: the smaller keeps up to half, the larger the rest.
func fitPayload(request, response string, limit int) (string, bool, string, bool) {
	if len(request)+len(response) <= limit {
		return request, false, response, false
	}
	half := limit / 2
	switch {
	case len(request) <= half:
		r, cut := cutUTF8(response, limit-len(request))
		return request, false, r, cut
	case len(response) <= half:
		r, cut := cutUTF8(request, limit-len(response))
		return r, cut, response, false
	}
	q, qc := cutUTF8(request, half)
	r, rc := cutUTF8(response, limit-half)
	return q, qc, r, rc
}
func cutUTF8(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	// A copy, so a queued span holds only what the payload budget counts.
	return strings.Clone(s[:n]), true
}

func (c *Client) mcpHandler(ctx context.Context, parent mcpspan.Span, active bool, method, name string, meta mcpspan.Meta, run func(context.Context) error) (err error) {
	m := &mcpMark{layer: "handler", step: !active}
	d := MilestoneContext{Surface: SurfaceMCP, Kind: mcpKind(method), MCP: &MCPContext{Method: method, Name: meta.RegisteredName}}
	m.add(0, mcpMethodAttr(method)...)
	if !active {
		d.MCP.Client, d.MCP.ClientVersion, d.MCP.Protocol = meta.Client, meta.ClientVersion, meta.Protocol
		// As the step, the handler span carries the session its transport span would.
		if meta.SessionID != "" {
			m.add(0, attribute.String("agentrail.mcp.session.id_hash", handoff.Hash(c.options.HashKey, meta.SessionID)))
		}
	}
	if parent != nil {
		ctx = parent.Graft(ctx)
	}
	ctx, span := c.start(ctx, name, d, m)
	if span == nil {
		return run(ctx)
	}
	defer func() {
		if p := recover(); p != nil {
			span.SetStatus(codes.Error, "")
			m.add(0, attribute.String("error.type", "panic"))
			span.End()
			panic(p)
		}
		if err != nil {
			span.SetStatus(codes.Error, "")
			if errors.As(err, &mcpspan.OperationError{}) {
				m.add(0, attribute.Bool("agentrail.mcp.result.is_error", true), attribute.String("error.type", "tool_error"))
			} else {
				m.add(0, attribute.String("error.type", bounded(reflect.TypeOf(err).String(), 4096)))
			}
		}
		span.End()
	}()
	return run(ctx)
}

// withoutPayload drops payload attributes from a queued span copy.
func (s sanitizedSpan) withoutPayload() sanitizedSpan {
	attrs := make([]attribute.KeyValue, 0, len(s.attrs))
	for _, a := range s.attrs {
		switch a.Key {
		case keyMCPRequestPayload, keyMCPResponsePayload, keyMCPRequestTruncated, keyMCPResponseTrunc:
		default:
			attrs = append(attrs, a)
		}
	}
	s.attrs = attrs
	s.size -= s.payload
	s.payload = 0
	return s
}
