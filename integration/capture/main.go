// Command capture produces synthetic OTLP bytes through the public Go SDK.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	agentrail "github.com/agentrail-co/agentrail-go"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type packet struct {
	Signal   string `json:"signal"`
	Protobuf string `json:"protobuf"`
}
type capture struct {
	Provenance       string   `json:"provenance"`
	Exports          []packet `json:"exports"`
	TraceID          string   `json:"traceId"`
	RootSpanID       string   `json:"rootSpanId"`
	MilestoneSpanID  string   `json:"milestoneSpanId"`
	SupportingSpanID string   `json:"supportingSpanId"`
	Expected         struct {
		Entries int `json:"entries"`
		Spans   int `json:"spans"`
		Logs    int `json:"logs"`
	} `json:"expected"`
}

func run(output string) (runErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	result := capture{Provenance: "synthetic public Go SDK; no provider calls or production credentials"}
	var collectorErrors []error
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-protobuf" {
			w.WriteHeader(415)
			return
		}
		data, err := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			collectorErrors = append(collectorErrors, err)
			w.WriteHeader(500)
			return
		}
		signal := strings.TrimPrefix(r.URL.Path, "/v1/")
		if signal != "traces" && signal != "logs" {
			w.WriteHeader(404)
			return
		}
		result.Exports = append(result.Exports, packet{Signal: signal, Protobuf: base64.StdEncoding.EncodeToString(data)})
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer collector.Close()
	var captureErrors []agentrail.CaptureError
	processors, err := agentrail.NewProcessors(ctx, agentrail.Options{
		Collector: agentrail.CollectorOptions{Endpoint: collector.URL, Secret: "synthetic-local-collector"},
		HashKey:   "synthetic-go-receiver-hash-key", ServiceName: "synthetic-go-service", ServiceInstanceID: "synthetic-go-instance",
		Queue: agentrail.QueueOptions{ScheduledDelay: time.Hour},
		IncomingLocation: func(*http.Request) (agentrail.IPLocation, error) {
			return agentrail.IPLocation{Country: "US", Region: "Washington", City: "Seattle", Source: agentrail.LocationConfigured}, nil
		},
		OnError: func(_ context.Context, e agentrail.CaptureError) {
			mu.Lock()
			captureErrors = append(captureErrors, e)
			mu.Unlock()
		},
	})
	if err != nil {
		return err
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(processors.Sampler(sdktrace.AlwaysSample())), sdktrace.WithSpanProcessor(processors.SpanProcessor))
	client, err := processors.Bind(provider)
	if err != nil {
		return errors.Join(err, provider.Shutdown(ctx))
	}
	defer func() {
		runErr = errors.Join(runErr, client.Shutdown(ctx), provider.Shutdown(ctx))
	}()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	outbound := &http.Client{Transport: client.Transport(nil)}
	var handlerErr error
	app := httptest.NewServer(client.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result.TraceID = trace.SpanContextFromContext(r.Context()).TraceID().String()
		result.RootSpanID = trace.SpanContextFromContext(r.Context()).SpanID().String()
		if err := client.Identify(r.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: "synthetic-go-user", ActingUserID: "synthetic-go-acting-user"}); err != nil {
			handlerErr = err
			w.WriteHeader(500)
			return
		}
		handlerErr = client.WithMilestone(r.Context(), "synthetic.checkout", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "checkout.confirm"}, func(ctx context.Context) error {
			result.MilestoneSpanID = trace.SpanContextFromContext(ctx).SpanID().String()
			client.Log(ctx, agentrail.StructuredLog{SeverityNumber: 9, SeverityText: "INFO", EventName: "synthetic.milestone"})
			ctx, supporting := provider.Tracer("synthetic-support").Start(ctx, "synthetic database work")
			defer supporting.End()
			result.SupportingSpanID = supporting.SpanContext().SpanID().String()
			client.Log(ctx, agentrail.StructuredLog{SeverityNumber: 17, SeverityText: "ERROR", ErrorType: "SyntheticError", EventName: "synthetic.supporting"})
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/synthetic-target", nil)
			if err != nil {
				return err
			}
			response, err := outbound.Do(request)
			if err != nil {
				return err
			}
			return response.Body.Close()
		})
		if handlerErr != nil {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	})))
	defer app.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, app.URL+"/synthetic-checkout", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	if err := response.Body.Close(); err != nil {
		return err
	}
	if handlerErr != nil {
		return handlerErr
	}
	if response.StatusCode != 204 {
		return fmt.Errorf("synthetic request status %d", response.StatusCode)
	}
	if err := client.Flush(ctx); err != nil {
		return err
	}
	if err := client.Shutdown(ctx); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if len(collectorErrors) > 0 {
		return errors.Join(collectorErrors...)
	}
	if len(captureErrors) > 0 {
		return fmt.Errorf("capture reported %d errors", len(captureErrors))
	}
	if len(result.Exports) < 2 {
		return fmt.Errorf("expected traces and logs exports; got %d packets", len(result.Exports))
	}
	result.Expected.Entries = 2
	result.Expected.Spans = 4
	result.Expected.Logs = 2
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(output, append(encoded, '\n'), 0600)
}
func main() {
	output := flag.String("output", "", "absolute or relative destination for synthetic capture")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "--output is required")
		os.Exit(2)
	}
	if err := run(*output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
