package agentrail_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	agentrail "github.com/agentrail-co/agentrail-go"
	"go.opentelemetry.io/otel/attribute"
	apilog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func attr(attrs []*common.KeyValue, k string) *common.AnyValue {
	for _, a := range attrs {
		if a.Key == k {
			return a.Value
		}
	}
	return nil
}
func spans(r *receiver) []*tracepb.Span {
	out := []*tracepb.Span{}
	for _, v := range r.traces {
		for _, rs := range v.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}
func TestExistingProviderSamplingAndPrivacyIsolation(t *testing.T) {
	r, s := collector(t)
	o := options(s.URL)
	o.Propagator = propagation.TraceContext{}
	capture, err := agentrail.NewProcessors(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	customer := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(resource.NewSchemaless(attribute.String("customer.resource", "private-resource"))), sdktrace.WithSampler(capture.Sampler(sdktrace.NeverSample())), sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(customer)), sdktrace.WithSpanProcessor(capture.SpanProcessor))
	defer tp.Shutdown(context.Background())
	c, err := capture.Bind(tp)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(resource.NewSchemaless(attribute.String("customer.secret", "private-resource"))), sdklog.WithProcessor(capture.LogProcessor))
	defer lp.Shutdown(context.Background())
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, sp := tp.Tracer("private-scope").Start(r.Context(), "private-operation", trace.WithAttributes(attribute.String("customer.secret", "private-attribute")))
		sp.AddEvent("private-event")
		defer sp.End()
		var record apilog.Record
		record.SetBody(attribute.StringValue("private-body"))
		lp.Logger("private-logger").Emit(ctx, record)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("HEAD", "http://app.test/private", nil))
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(spans(r)) != 2 {
		t.Fatalf("lost record-only spans: %d", len(spans(r)))
	}
	if len(customer.GetSpans()) != 0 {
		t.Fatal("changed customer sampling")
	}
	for _, x := range r.traces {
		if strings.Contains(fmt.Sprint(x), "private-") {
			t.Fatal("private span field leaked")
		}
		for _, rs := range x.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					if sp.Flags&1 != 0 {
						t.Fatal("promoted sampled flag")
					}
				}
			}
		}
	}
	for _, x := range r.logs {
		if strings.Contains(fmt.Sprint(x), "private-") {
			t.Fatal("private log/resource leaked")
		}
	}
	// A customer provider remains operational after Agentrail shutdown.
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, sp := tp.Tracer("customer").Start(context.Background(), "still-running", trace.WithNewRoot())
	if sp.IsRecording() {
		t.Fatal("NeverSample unexpectedly recording")
	}
	sp.End()
}
func TestIdentityIsolationAndConflicts(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		id := q.Header.Get("X-Synthetic-Identity")
		if err := c.Identify(q.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: id}); err != nil {
			t.Error(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.Identify(q.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: id})
				c.Milestone(q.Context(), "work", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "work"})
			}()
		}
		wg.Wait()
		c.Identify(q.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: id + "-conflict"})
	}))
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			q := httptest.NewRequest("GET", "http://app.test/", nil)
			q.Header.Set("X-Synthetic-Identity", id)
			h.ServeHTTP(httptest.NewRecorder(), q)
		}(id)
	}
	wg.Wait()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]string{}
	conflicts := 0
	for _, sp := range spans(r) {
		v := attr(sp.Attributes, "agentrail.principal.id_hash")
		if v == nil {
			t.Fatal("missing identity")
		}
		key := string(sp.TraceId)
		if sp.Name == "identity.asserted" {
			conflicts++
			continue
		}
		if before, ok := seen[key]; ok && before != v.GetStringValue() {
			t.Fatal("identity drift")
		}
		seen[key] = v.GetStringValue()
	}
	if len(seen) != 2 || conflicts != 2 {
		t.Fatalf("wrong roots/conflicts %d/%d", len(seen), conflicts)
	}
	var ids []string
	for _, id := range seen {
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Fatal("cross-request identity leak")
	}
}
func TestMilestonePreservesErrorsPanicsAndChildScope(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	sentinel := errors.New("private-failure")
	if got := c.WithMilestone(context.Background(), "operation", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "operation"}, func(ctx context.Context) error { return sentinel }); got != sentinel {
		t.Fatal("replaced error")
	}
	func() {
		defer func() {
			if got := recover(); got != sentinel {
				t.Fatal("replaced panic")
			}
		}()
		c.WithMilestone(context.Background(), "panic", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "operation"}, func(context.Context) error { panic(sentinel) })
	}()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(spans(r)) != 2 {
		t.Fatal("missing steps")
	}
	for _, sp := range spans(r) {
		if sp.Status.Code != 2 {
			t.Fatal("missing failure status")
		}
		if strings.Contains(fmt.Sprint(sp), "private-failure") {
			t.Fatal("error message exported")
		}
	}
}
