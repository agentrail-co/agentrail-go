package slog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	agentrailslog "github.com/agentrail-co/agentrail-go/slog"
	"go.opentelemetry.io/otel/trace"
	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

type collector struct {
	mu   sync.Mutex
	logs []*logpb.LogRecord
}

func newCapture(t *testing.T, fields ...string) (*agentrail.Client, *collector) {
	t.Helper()
	r := new(collector)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.HasSuffix(req.URL.Path, "/logs") {
			message := new(collectorlog.ExportLogsServiceRequest)
			if err := proto.Unmarshal(raw, message); err != nil {
				t.Error(err)
				return
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			for _, resource := range message.ResourceLogs {
				for _, scope := range resource.ScopeLogs {
					r.logs = append(r.logs, scope.LogRecords...)
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	client, err := agentrail.New(context.Background(), agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: server.URL, Secret: "synthetic-source"}, HashKey: "synthetic-key", ServiceName: "slog-test", Logs: agentrail.LogsOptions{IncludeFields: fields}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return client, r
}
func logs(t *testing.T, c *agentrail.Client, r *collector) []*logpb.LogRecord {
	t.Helper()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*logpb.LogRecord(nil), r.logs...)
}
func attr(r *logpb.LogRecord, key string) string {
	for _, a := range r.Attributes {
		if a.Key == key {
			return a.Value.GetStringValue()
		}
	}
	return ""
}

// This fails if the adapter exports message text by default, loses caller context,
// or replaces the application's existing handler output.
func TestComposesApplicationOutputAndCorrelatesWithoutMessage(t *testing.T) {
	c, receiver := newCapture(t)
	var actual, want bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	wrapped := agentrailslog.NewHandler(c, slog.NewJSONHandler(&actual, opts))
	baseline := slog.NewJSONHandler(&want, opts)
	record := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelError, "private-message", 0)
	record.AddAttrs(slog.String("event.name", "checkout.failed"), slog.Any("error", errors.New("private-error")), slog.String("password", "private-password"))
	ctx, end := c.StartMilestone(context.Background(), "checkout", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "test"})
	sc := trace.SpanContextFromContext(ctx)
	if err := wrapped.Handle(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := baseline.Handle(ctx, record); err != nil {
		t.Fatal(err)
	}
	end(nil)
	if actual.String() != want.String() {
		t.Fatalf("application output changed: %s != %s", actual.String(), want.String())
	}
	got := logs(t, c, receiver)
	if len(got) != 1 {
		t.Fatalf("got %d logs", len(got))
	}
	log := got[0]
	traceID := sc.TraceID()
	if !bytes.Equal(log.TraceId, traceID[:]) {
		t.Fatalf("wrong trace: %x", log.TraceId)
	}
	if log.Body.GetStringValue() != "" {
		t.Fatalf("unexpected body: %v", log.Body)
	}
	if log.SeverityNumber != 17 || log.SeverityText != "ERROR" || log.EventName != "checkout.failed" || attr(log, "error.type") != "*errors.errorString" {
		t.Fatalf("missing structural fields: %v", log)
	}
	for _, secret := range []string{"private-message", "private-error", "private-password"} {
		if strings.Contains(log.String(), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
}

// This fails if a derived handler mutates its parent or flattens attrs under the
// current group instead of the group that was active when WithAttrs was called.
func TestGroupsAttrsAndSelectedScalars(t *testing.T) {
	c, receiver := newCapture(t, "message", "tenant", "request.name", "request.deep.code", "request.safe", "request.object")
	h := agentrailslog.NewHandler(c, slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	parent := slog.New(h).With("tenant", "public-tenant")
	grouped := parent.WithGroup("request").With("name", "stable-name").WithGroup("deep")
	grouped.Info("selected-message", "code", 42)
	parent.WithGroup("request").Info("other", "safe", true, "object", map[string]string{"private": "secret"})
	parent.Info("parent-message")
	got := logs(t, c, receiver)
	if len(got) != 3 {
		t.Fatalf("got %d logs", len(got))
	}
	want := []map[string]any{{"message": "selected-message", "tenant": "public-tenant", "request.name": "stable-name", "request.deep.code": float64(42)}, {"message": "other", "tenant": "public-tenant", "request.safe": true}, {"message": "parent-message", "tenant": "public-tenant"}}
	for i, record := range got {
		var fields map[string]any
		if err := json.Unmarshal([]byte(record.Body.GetStringValue()), &fields); err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(fields)
		b, _ := json.Marshal(want[i])
		if !bytes.Equal(a, b) {
			t.Fatalf("log %d: %s != %s", i, a, b)
		}
	}
}

// This fails if the adapter enables levels the existing handler rejects, or
// maps slog's four standard levels to the wrong OpenTelemetry severity.
func TestEnabledAndSeverity(t *testing.T) {
	c, receiver := newCapture(t)
	h := agentrailslog.NewHandler(c, slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger := slog.New(h)
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		logger.Log(context.Background(), level, "private")
	}
	got := logs(t, c, receiver)
	if len(got) != 4 {
		t.Fatalf("got %d logs", len(got))
	}
	for i, want := range []int32{5, 9, 13, 17} {
		if int32(got[i].SeverityNumber) != want {
			t.Fatalf("severity %d = %d", i, got[i].SeverityNumber)
		}
	}
	disabled := agentrailslog.NewHandler(c, slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if disabled.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("changed downstream enabled decision")
	}
	slog.New(disabled).Info("disabled")
	if len(logs(t, c, receiver)) != 4 {
		t.Fatal("captured disabled log")
	}
}

type changingValue struct{ calls *int }

func (v changingValue) LogValue() slog.Value { *v.calls++; return slog.IntValue(*v.calls) }

// This fails if capture resolves application LogValuers in addition to the
// downstream handler, changing its output or executing application code twice.
func TestApplicationLogValuerEvaluatedOnlyByNext(t *testing.T) {
	c, receiver := newCapture(t, "dynamic")
	var output bytes.Buffer
	calls := 0
	logger := slog.New(agentrailslog.NewHandler(c, slog.NewJSONHandler(&output, nil)))
	logger.Info("private", "dynamic", changingValue{&calls})
	if calls != 1 {
		t.Fatalf("LogValue called %d times", calls)
	}
	if !strings.Contains(output.String(), `"dynamic":1`) {
		t.Fatalf("changed application output: %s", output.String())
	}
	got := logs(t, c, receiver)
	if len(got) != 1 || got[0].Body.GetStringValue() != "" {
		t.Fatalf("deferred value captured: %v", got)
	}
}

type failingHandler struct{ err error }

func (h failingHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h failingHandler) Handle(context.Context, slog.Record) error { return h.err }
func (h failingHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h failingHandler) WithGroup(string) slog.Handler             { return h }

// This fails if capture hides an application handler failure.
func TestHandlerErrorPreserved(t *testing.T) {
	c, receiver := newCapture(t)
	original := errors.New("synthetic application sink failure")
	h := agentrailslog.NewHandler(c, failingHandler{original})
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "private", 0)); err != original {
		t.Fatalf("changed handler error: %v", err)
	}
	if len(logs(t, c, receiver)) != 1 {
		t.Fatal("capture lost when application sink failed")
	}
}

// This fails if the handler discards the suppressed context supplied to
// diagnostics, causing a diagnostic log to recursively invoke OnError.
func TestDiagnosticLogsDoNotRecursivelyCapture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	var logger *slog.Logger
	calls := 0
	c, err := agentrail.New(context.Background(), agentrail.Options{Collector: agentrail.CollectorOptions{Endpoint: server.URL, Secret: "synthetic-source"}, HashKey: "synthetic-key", ServiceName: "slog-test", OnError: func(ctx context.Context, e agentrail.CaptureError) {
		calls++
		if calls > 1 {
			t.Error("recursive diagnostics")
			return
		}
		logger.ErrorContext(ctx, e.Error())
	}})
	if err != nil {
		t.Fatal(err)
	}
	logger = slog.New(agentrailslog.NewHandler(c, slog.NewJSONHandler(io.Discard, nil)))
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	logger.Info("after shutdown")
	if calls != 1 {
		t.Fatalf("got %d diagnostics", calls)
	}
}
