// Command beside-otel adds Agentrail to a server that already traces itself
// with OpenTelemetry. Nothing in that setup changes: otelhttp still wraps the
// handler and the client transport, and Agentrail's middleware and transport
// go inside them.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The application's own tracing, configured by the usual OTEL_* variables.
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	// Agentrail reads none of the globals above and registers none of its own.
	c, err := agentrail.New(ctx, agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: os.Getenv("AGENTRAIL_ENDPOINT"), Secret: os.Getenv("AGENTRAIL_SOURCE_SECRET")}, HashKey: os.Getenv("AGENTRAIL_HASH_KEY"), ServiceName: "orders-api", OutboundOrigins: []string{"https://inventory.example.com"}})
	if err != nil {
		return errors.Join(err, tp.Shutdown(ctx))
	}
	inventory := &http.Client{Transport: otelhttp.NewTransport(c.Transport(nil))}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		// The application's spans stay children of otelhttp's server span.
		ctx, span := otel.Tracer("orders").Start(r.Context(), "reserve stock")
		defer span.End()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://inventory.example.com/reservations", nil)
		if err != nil {
			http.Error(w, "reservation failed", http.StatusInternalServerError)
			return
		}
		response, err := inventory.Do(request)
		if err != nil {
			http.Error(w, "reservation failed", http.StatusBadGateway)
			return
		}
		response.Body.Close()
		w.WriteHeader(http.StatusCreated)
	})
	server := &http.Server{Addr: ":8080", Handler: otelhttp.NewHandler(c.Middleware(mux), "orders-api"), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return errors.Join(err, server.Shutdown(shutdown), c.Shutdown(shutdown), tp.Shutdown(shutdown))
}
