package agentrail

import (
	"context"
	"reflect"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Surface string

const (
	SurfaceHTTP     Surface = "http"
	SurfaceMCP      Surface = "mcp"
	SurfaceAuth     Surface = "auth"
	SurfaceCustom   Surface = "custom"
	SurfaceA2A      Surface = "a2a"
	SurfaceGenAI    Surface = "genai"
	SurfacePayment  Surface = "payment"
	SurfaceEdge     Surface = "edge"
	SurfaceFirewall Surface = "firewall"
	SurfaceBrowser  Surface = "browser"
)

type MCPContext struct{ Method, Client, ClientVersion, Protocol string }
type MilestoneContext struct {
	Surface Surface
	Kind    string
	MCP     *MCPContext
	// Performance is explicit evidence supplied after application authorization;
	// it is never inferred from tool inputs or results, and applies only to this
	// milestone's own span.
	Performance *PerformanceContext
}

func validSurface(s Surface) bool {
	switch s {
	case SurfaceHTTP, SurfaceMCP, SurfaceAuth, SurfaceCustom, SurfaceA2A, SurfaceGenAI, SurfacePayment, SurfaceEdge, SurfaceFirewall, SurfaceBrowser:
		return true
	}
	return false
}

// StartMilestone starts a scoped operation. Call the returned end function once
// with the application error. Use WithMilestone to preserve panics automatically.
func (c *Client) StartMilestone(ctx context.Context, name string, d MilestoneContext) (context.Context, func(error)) {
	ctx, span := c.start(ctx, name, d, nil)
	if span == nil {
		return ctx, func(error) {}
	}
	return ctx, func(err error) {
		if err != nil {
			span.SetStatus(codes.Error, "")
			span.SetAttributes(attribute.String("error.type", bounded(reflect.TypeOf(err).String(), 4096)))
		}
		span.End()
	}
}

// start opens a milestone span, or returns a nil span after reporting why not.
// An mcp mark adds MCP attributes and can make the span no step.
func (c *Client) start(ctx context.Context, name string, d MilestoneContext, mcp *mcpMark) (context.Context, trace.Span) {
	if suppressed(ctx) {
		return ctx, nil
	}
	if err := c.ready(); err != nil {
		c.report(ctx, "closed", "traces", 1)
		return ctx, nil
	}
	if name == "" || d.Kind == "" || !validText(name, 256) || !validText(d.Kind, 256) || !validSurface(d.Surface) {
		c.report(ctx, "invalid_milestone", "context", 1)
		return ctx, nil
	}
	if d.MCP != nil {
		copy := *d.MCP
		d.MCP = &copy
	}
	var performance []attribute.KeyValue
	if d.Performance != nil {
		var ok bool
		// Invalid evidence is omitted and reported; the milestone still runs.
		if performance, ok = performanceAttributes(*d.Performance, c.options.HashKey); !ok {
			c.report(ctx, "invalid_performance", "context", 1)
		}
		d.Performance = nil
	}
	if c.state(ctx) == nil {
		external := trace.SpanContextFromContext(ctx)
		s := &requestState{}
		if external.IsValid() {
			s.links = []trace.Link{{SpanContext: cleanSC(external)}}
		}
		ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
		ctx = context.WithValue(ctx, stateKey{c}, s)
	}
	m := &mark{name: name, details: d, performance: performance, mcp: mcp}
	links := []trace.Link{}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		links = c.state(ctx).links
	}
	ctx, span := c.tp.Tracer("agentrail-go").Start(context.WithValue(ctx, markKey{c}, m), name, trace.WithLinks(links...))
	// A mark applies only to the created span; supporting children remain ordinary spans.
	ctx = context.WithValue(ctx, markKey{c}, (*mark)(nil))
	if !span.IsRecording() {
		c.report(ctx, "entry_not_recording", "traces", 1)
	}
	return ctx, span
}
func (c *Client) Milestone(ctx context.Context, name string, d MilestoneContext) {
	_, end := c.StartMilestone(ctx, name, d)
	end(nil)
}

// WithMilestone returns the application's original error and rethrows panics.
func (c *Client) WithMilestone(ctx context.Context, name string, d MilestoneContext, run func(context.Context) error) (err error) {
	ctx, end := c.StartMilestone(ctx, name, d)
	defer func() {
		if p := recover(); p != nil {
			span := trace.SpanFromContext(ctx)
			span.SetStatus(codes.Error, "")
			span.SetAttributes(attribute.String("error.type", "panic"))
			end(nil)
			panic(p)
		}
		end(err)
	}()
	return run(ctx)
}
