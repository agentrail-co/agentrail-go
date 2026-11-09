package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"

	agentrail "github.com/agentrail-co/agentrail-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	ctx := context.Background()
	p, err := agentrail.NewProcessors(ctx, agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: os.Getenv("AGENTRAIL_ENDPOINT"), Secret: os.Getenv("AGENTRAIL_SOURCE_SECRET")}, HashKey: os.Getenv("AGENTRAIL_HASH_KEY"), ServiceName: "composed-api", Propagator: propagation.TraceContext{}})
	if err != nil {
		return err
	}
	// Keep customer processors/exporters here alongside the Agentrail processor.
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "composed-api"))), sdktrace.WithSampler(p.Sampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1)))), sdktrace.WithSpanProcessor(p.SpanProcessor))
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(p.LogProcessor))
	c, err := p.Bind(tp)
	if err != nil {
		return errors.Join(err, tp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	// Pass tp to other retained instrumentation and p.Propagator to retained
	// propagation integration. This example does not install process-wide globals.
	handler := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tp.Tracer("customer").Start(r.Context(), "database.operation")
		defer span.End()
		c.Log(ctx, agentrail.StructuredLog{EventName: "request.handled", SeverityNumber: 9})
		w.WriteHeader(204)
	}))
	_ = handler
	// The hosting application drains requests before this shutdown sequence.
	return errors.Join(c.Shutdown(ctx), lp.Shutdown(ctx), tp.Shutdown(ctx))
}
