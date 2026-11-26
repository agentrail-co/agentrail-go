// Command capture produces synthetic OTLP bytes through the public Go SDK.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
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
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
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
	client, err := agentrail.New(ctx, agentrail.Options{
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
	defer func() {
		runErr = errors.Join(runErr, client.Shutdown(ctx))
	}()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	outbound := &http.Client{Transport: client.Transport(nil)}
	var handlerErr error
	app := httptest.NewServer(client.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := client.Identify(r.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: "synthetic-go-user", ActingUserID: "synthetic-go-acting-user"}); err != nil {
			handlerErr = err
			w.WriteHeader(500)
			return
		}
		handlerErr = client.WithMilestone(r.Context(), "synthetic.checkout", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "checkout.confirm"}, func(ctx context.Context) error {
			client.Log(ctx, agentrail.StructuredLog{SeverityNumber: 9, SeverityText: "INFO", EventName: "synthetic.milestone"})
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/synthetic-target", nil)
			if err != nil {
				return err
			}
			response, err := outbound.Do(request)
			if err != nil {
				return err
			}
			// The outbound call's span is the supporting span beneath the
			// milestone. The response's request is the one Agentrail sent, so
			// its context carries that span and this log is correlated to it.
			client.Log(response.Request.Context(), agentrail.StructuredLog{SeverityNumber: 17, SeverityText: "ERROR", ErrorType: "SyntheticError", EventName: "synthetic.supporting"})
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
	if err := identify(&result); err != nil {
		return err
	}
	result.Expected.Entries = 2
	result.Expected.Spans = 3
	result.Expected.Logs = 2
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(output, append(encoded, '\n'), 0600)
}

// identify reads the IDs the receiver test checks from the exported spans:
// the request's entry, its milestone, and the outbound call beneath it.
func identify(result *capture) error {
	var root, milestone, supporting *tracepb.Span
	count := 0
	for _, packet := range result.Exports {
		if packet.Signal != "traces" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(packet.Protobuf)
		if err != nil {
			return err
		}
		export := new(collectortrace.ExportTraceServiceRequest)
		if err := proto.Unmarshal(raw, export); err != nil {
			return err
		}
		for _, resource := range export.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				for _, span := range scope.Spans {
					count++
					switch {
					case span.Kind == tracepb.Span_SPAN_KIND_SERVER:
						root = span
					case span.Name == "synthetic.checkout":
						milestone = span
					case span.Kind == tracepb.Span_SPAN_KIND_CLIENT:
						supporting = span
					}
				}
			}
		}
	}
	if count != 3 || root == nil || milestone == nil || supporting == nil {
		return fmt.Errorf("expected an entry, a milestone and an outbound call; got %d spans", count)
	}
	result.TraceID = hex.EncodeToString(root.TraceId)
	result.RootSpanID = hex.EncodeToString(root.SpanId)
	result.MilestoneSpanID = hex.EncodeToString(milestone.SpanId)
	result.SupportingSpanID = hex.EncodeToString(supporting.SpanId)
	return nil
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
