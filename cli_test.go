// Copyright 2026 Agentrail. Licensed under the Apache License, Version 2.0.
package agentrail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	agentrail "github.com/agentrail-co/agentrail-go"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

type cliContract struct {
	CLIHeader struct {
		Vectors struct {
			Header struct {
				Value string         `json:"value"`
				Reads map[string]any `json:"reads"`
			} `json:"header"`
			Unread       []string `json:"unread"`
			KeyedSession struct {
				HashKey, Session, Hash string
			} `json:"keyedSession"`
		} `json:"vectors"`
	} `json:"cliHeader"`
	OTLP struct {
		CLIAttributes map[string]string `json:"cliAttributes"`
	} `json:"otlp"`
}

func attributeValue(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	}
	return nil
}

// This fails if someone stops reading the header, sends the session as the CLI
// gave it, puts the facts under names the collector does not read, or on a
// span other than the request's own: a CLI's runs would join no journey, or
// its session would leave the server unhashed. It fails too if a header that
// does not read, or one sent twice, sets anything.
func TestMiddlewareRecordsWhatACLIHeaderSays(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract cliContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	vectors, names := contract.CLIHeader.Vectors, contract.OTLP.CLIAttributes
	r, s := collector(t)
	o := options(s.URL)
	o.HashKey = vectors.KeyedSession.HashKey
	c, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	app := httptest.NewServer(c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})))
	defer app.Close()
	send := func(path string, values ...string) {
		q, err := http.NewRequest(http.MethodPost, app.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			q.Header.Add("x-agentrail-cli", value)
		}
		resp, err := http.DefaultClient.Do(q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	send("/cli", vectors.Header.Value)
	send("/plain")
	for i, value := range vectors.Unread {
		if value != "" {
			send(fmt.Sprintf("/unread/%d", i), value)
		}
	}
	send("/twice", vectors.Header.Value, vectors.Header.Value)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	byPath := map[string]map[string]any{}
	for _, export := range r.traces {
		for _, rs := range export.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, span := range ss.Spans {
					attrs := map[string]any{}
					for _, kv := range span.Attributes {
						attrs[kv.Key] = attributeValue(kv.Value)
					}
					url, _ := attrs["url.full"].(string)
					cli := map[string]any{}
					for key, value := range attrs {
						if strings.HasPrefix(key, "agentrail.cli.") {
							cli[key] = value
						}
					}
					byPath[strings.TrimPrefix(url, app.URL)] = cli
				}
			}
		}
	}
	reads := vectors.Header.Reads
	evidence := []string{}
	for _, name := range reads["evidence"].([]any) {
		evidence = append(evidence, name.(string))
	}
	want := map[string]any{
		names["name"]:         reads["cli"],
		names["version"]:      reads["cliVersion"],
		names["command"]:      reads["command"],
		names["run"]:          reads["run"],
		names["sequence"]:     int64(reads["sequence"].(float64)),
		names["mode"]:         reads["mode"],
		names["sessionHash"]:  vectors.KeyedSession.Hash,
		names["sessionFrom"]:  reads["sessionFrom"],
		names["agent"]:        reads["agent"],
		names["agentVersion"]: reads["agentVersion"],
		names["evidence"]:     strings.Join(evidence, ","),
		names["sdkVersion"]:   reads["sdkVersion"],
	}
	if got := byPath["/cli"]; !reflect.DeepEqual(got, want) {
		t.Errorf("CLI request recorded %v, want %v", got, want)
	}
	if reads["session"] != vectors.KeyedSession.Session {
		t.Fatal("the contract's header names another session than its keyed vector")
	}
	unread := 0
	for path, cli := range byPath {
		if path == "/cli" {
			continue
		}
		if strings.HasPrefix(path, "/unread/") {
			unread++
		}
		if len(cli) != 0 {
			t.Errorf("%s recorded %v", path, cli)
		}
	}
	if unread != len(vectors.Unread)-1 || byPath["/twice"] == nil || byPath["/plain"] == nil {
		t.Fatalf("missing spans: %v", byPath)
	}
	for _, export := range r.traces {
		if strings.Contains(fmt.Sprint(export), vectors.KeyedSession.Session) {
			t.Fatal("export carried the CLI's session unhashed")
		}
	}
}

// This fails if someone copies a CLI's facts onto the calls a request makes,
// or lets the server send the header on: the call would be a second CLI step,
// and the next service would learn the CLI's run and session though the CLI
// never listed it.
func TestCLIHeaderStaysOnTheRequestsOwnSpan(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract cliContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	// The application's own tracing, which Agentrail neither records nor touches.
	tp := sdktrace.NewTracerProvider()
	defer tp.Shutdown(context.Background())
	var received []string
	downstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, q *http.Request) {
		received = append(received, q.Header.Get("x-agentrail-cli"))
	}))
	defer downstream.Close()
	outbound := &http.Client{Transport: c.Transport(nil)}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		// A library's own server span inside the request, as an RPC layer opens one.
		ctx, inner := tp.Tracer("a-library").Start(q.Context(), "inner", trace.WithSpanKind(trace.SpanKindServer))
		defer inner.End()
		forward, err := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL+"/forwarded", nil)
		if err != nil {
			t.Error(err)
			return
		}
		forward.Header.Set("x-agentrail-cli", q.Header.Get("x-agentrail-cli"))
		resp, err := outbound.Do(forward)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}))
	q := httptest.NewRequest(http.MethodPost, "http://app.test/cli", nil)
	q.Header.Set("x-agentrail-cli", contract.CLIHeader.Vectors.Header.Value)
	h.ServeHTTP(httptest.NewRecorder(), q)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0] != "" {
		t.Fatalf("downstream received %q", received)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var withCLI []string
	for _, export := range r.traces {
		for _, rs := range export.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, span := range ss.Spans {
					for _, kv := range span.Attributes {
						if strings.HasPrefix(kv.Key, "agentrail.cli.") {
							withCLI = append(withCLI, span.Name)
							break
						}
					}
				}
			}
		}
	}
	if !reflect.DeepEqual(withCLI, []string{"HTTP POST"}) {
		t.Fatalf("CLI facts on %v", withCLI)
	}
}
