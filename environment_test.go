package agentrail_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// This fails if our exporters or providers take anything from the
// application's OTEL_* variables, which configure its own OpenTelemetry: a
// vendor's key would reach our collector, our exports would be compressed or
// cut off by its timeout, our spans sampled away, or our attributes cut to
// its limits.
func TestNewTakesNothingFromTheApplicationsOTELEnvironment(t *testing.T) {
	for _, signal := range []string{"", "TRACES_", "LOGS_"} {
		t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"HEADERS", "x-vendor-key=private-vendor-key")
		t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"COMPRESSION", "gzip")
		t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"TIMEOUT", "1")
	}
	for _, name := range []string{"OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT", "OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT", "OTEL_LOGRECORD_ATTRIBUTE_VALUE_LENGTH_LIMIT"} {
		t.Setenv(name, "4")
	}
	for _, name := range []string{"OTEL_ATTRIBUTE_COUNT_LIMIT", "OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "OTEL_LOGRECORD_ATTRIBUTE_COUNT_LIMIT"} {
		t.Setenv(name, "1")
	}
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
	var mu sync.Mutex
	headers := []http.Header{}
	traces := []*collectortrace.ExportTraceServiceRequest{}
	logs := []*collectorlog.ExportLogsServiceRequest{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		// Longer than the application's 1 ms export timeout.
		time.Sleep(20 * time.Millisecond)
		raw, err := io.ReadAll(q.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		headers = append(headers, q.Header.Clone())
		if strings.HasSuffix(q.URL.Path, "/traces") {
			v := new(collectortrace.ExportTraceServiceRequest)
			if proto.Unmarshal(raw, v) == nil {
				traces = append(traces, v)
			}
		} else {
			v := new(collectorlog.ExportLogsServiceRequest)
			if proto.Unmarshal(raw, v) == nil {
				logs = append(logs, v)
			}
		}
	}))
	defer collector.Close()
	c, err := agentrail.New(context.Background(), options(collector.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	errorType := "SyntheticPaymentDeclinedError"
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		c.Log(q.Context(), agentrail.StructuredLog{SeverityNumber: 17, ErrorType: errorType, EventName: "checkout.failed"})
	}))
	in := httptest.NewRequest(http.MethodGet, "/orders/synthetic-order", nil)
	in.Header.Set("User-Agent", "synthetic-agent/1.0")
	h.ServeHTTP(httptest.NewRecorder(), in)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range headers {
		if h.Get("x-vendor-key") != "" || h.Get("Content-Encoding") != "" || h.Get("Authorization") != "Bearer synthetic-source" {
			t.Fatalf("collector request carried %v", h)
		}
	}
	if d := c.Diagnostics(); d.Spans.Exported != 1 || d.Logs.Exported != 1 || len(traces) != 1 || len(logs) != 1 {
		t.Fatalf("exports: %+v, %d trace and %d log requests decoded", d, len(traces), len(logs))
	}
	span := traces[0].ResourceSpans[0].ScopeSpans[0].Spans[0]
	for key, want := range map[string]string{"url.full": "http://example.com/orders/synthetic-order", "user_agent.original": "synthetic-agent/1.0", "http.request.method": "GET"} {
		if got := attr(span.Attributes, key).GetStringValue(); got != want {
			t.Fatalf("span %s = %q, want %q", key, got, want)
		}
	}
	record := logs[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if attr(record.Attributes, "error.type").GetStringValue() != errorType || len(attr(record.Attributes, "agentrail.log.id").GetStringValue()) != 32 {
		t.Fatalf("log attributes cut: %v", record.Attributes)
	}
}

// This fails if our exporters read the application's
// OTEL_EXPORTER_OTLP_CERTIFICATE or its client certificate and key: a
// collector that only the application's CA trusts would receive our exports,
// presented with the application's client certificate.
func TestNewIgnoresTheApplicationsOTELCertificates(t *testing.T) {
	var mu sync.Mutex
	received, presented := 0, 0
	collector := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		received++
		mu.Unlock()
	}))
	collector.TLS = &tls.Config{ClientAuth: tls.RequestClientCert, VerifyPeerCertificate: func(certs [][]byte, _ [][]*x509.Certificate) error {
		mu.Lock()
		presented += len(certs)
		mu.Unlock()
		return nil
	}}
	collector.StartTLS()
	defer collector.Close()
	dir := t.TempDir()
	write := func(name, kind string, der []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "application"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", write("ca.pem", "CERTIFICATE", collector.Certificate().Raw))
	t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", write("client.pem", "CERTIFICATE", cert))
	t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_KEY", write("client-key.pem", "PRIVATE KEY", keyDER))
	o := options(collector.URL)
	var codes []string
	o.OnError = func(_ context.Context, e agentrail.CaptureError) {
		mu.Lock()
		codes = append(codes, e.Code)
		mu.Unlock()
	}
	c, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	c.Milestone(context.Background(), "tls.check", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "tls.check"})
	c.Log(context.Background(), agentrail.StructuredLog{EventName: "tls.check"})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if received != 0 || presented != 0 {
		t.Fatalf("a collector only the application's CA trusts got %d exports and %d client certificates", received, presented)
	}
	if !slices.Contains(codes, "export_failed") {
		t.Fatalf("the refused exports were not reported: %v", codes)
	}
}
