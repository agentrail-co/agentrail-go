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
	c, err := agentrail.New(ctx, agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: os.Getenv("AGENTRAIL_ENDPOINT"), Secret: os.Getenv("AGENTRAIL_SOURCE_SECRET")}, HashKey: os.Getenv("AGENTRAIL_HASH_KEY"), ServiceName: "orders-api", OnError: func(ctx context.Context, e agentrail.CaptureError) {
		slog.ErrorContext(ctx, "capture issue", "code", e.Code, "signal", e.Signal, "count", e.Count)
	}})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
		err := c.WithMilestone(r.Context(), "checkout.confirm", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "checkout.confirm"}, func(ctx context.Context) error {
			c.Log(ctx, agentrail.StructuredLog{EventName: "checkout.confirmed", SeverityNumber: 9})
			return nil
		})
		if err != nil {
			http.Error(w, "checkout failed", 500)
			return
		}
		w.WriteHeader(201)
	})
	server := &http.Server{Addr: ":8080", Handler: c.Middleware(mux), ReadHeaderTimeout: 5 * time.Second}
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
	return errors.Join(err, server.Shutdown(shutdown), c.Shutdown(shutdown))
}
