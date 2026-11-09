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
	apilog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type stateKey struct{ client *Client }
type markKey struct{ client *Client }
type requestState struct {
	mu       sync.Mutex
	identity []attribute.KeyValue
	verified bool
	links    []trace.Link
}

func (s *requestState) principal() []attribute.KeyValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]attribute.KeyValue(nil), s.identity...)
}

type mark struct {
	name     string
	details  MilestoneContext
	identity []attribute.KeyValue
}
type spanMeta struct {
	state *requestState
	mark  *mark
}
type Client struct {
	options             Options
	resource            *resource.Resource
	tp                  trace.TracerProvider
	canonicalLogs       *sdklog.LoggerProvider
	logger              apilog.Logger
	spans               *delivery.Queue[sdktrace.ReadOnlySpan]
	logs                *delivery.Queue[sdklog.Record]
	metadata            sync.Map
	bound               atomic.Bool
	closed              atomic.Bool
	ownProvider         *sdktrace.TracerProvider
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
func (c *Client) ready() error {
	if c.closed.Load() {
		return ErrClosed
	}
	if !c.bound.Load() {
		return ErrUnbound
	}
	return nil
}
func New(ctx context.Context, options Options) (*Client, error) {
	p, err := NewProcessors(ctx, options)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(p.client.resource), sdktrace.WithSampler(p.Sampler(sdktrace.AlwaysSample())), sdktrace.WithSpanProcessor(p.SpanProcessor))
	c, err := p.Bind(tp)
	if err != nil {
		return nil, errors.Join(err, p.client.Shutdown(ctx), tp.Shutdown(ctx))
	}
	c.ownProvider = tp
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
			c.shutdownErr = errors.Join(c.shutdownErr, c.canonicalLogs.Shutdown(drain))
			if c.ownProvider != nil {
				c.shutdownErr = errors.Join(c.shutdownErr, c.ownProvider.Shutdown(drain))
			}
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
