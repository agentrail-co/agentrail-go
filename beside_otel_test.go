package agentrail_test

import (
	"bytes"
	"context"
	"testing"

	agentrail "github.com/agentrail-co/agentrail-go"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// This fails if a milestone outside a request takes the application's span
// out of the context its work runs in, stops linking to it, or counts itself
// among that span's children.
func TestMilestoneOutsideARequestKeepsTheApplicationsSpan(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer provider.Shutdown(context.Background())
	ctx, job := provider.Tracer("application").Start(context.Background(), "application.job")
	err = c.WithMilestone(ctx, "nightly.sync", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "sync"}, func(ctx context.Context) error {
		if trace.SpanFromContext(ctx) != job {
			t.Error("the application's span is no longer current inside the milestone")
		}
		_, step := provider.Tracer("application").Start(ctx, "application.step")
		step.End()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	job.End()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	theirs := map[string]tracetest.SpanStub{}
	for _, s := range exporter.GetSpans() {
		theirs[s.Name] = s
	}
	if theirs["application.step"].Parent.SpanID() != job.SpanContext().SpanID() {
		t.Fatalf("the application's step is not a child of its job: %v", theirs["application.step"].Parent)
	}
	if n := theirs["application.job"].ChildSpanCount; n != 1 {
		t.Fatalf("the application's job counts %d children, its step alone", n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ours := spans(r)
	jobID := job.SpanContext().SpanID()
	if len(ours) != 1 || len(ours[0].ParentSpanId) != 0 || len(ours[0].Links) != 1 || !bytes.Equal(ours[0].Links[0].SpanId, jobID[:]) {
		t.Fatalf("milestone is not a root linked to the application's span: %v", ours)
	}
}
