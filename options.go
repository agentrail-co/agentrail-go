// Copyright 2026 Agentrail. Licensed under the Apache License, Version 2.0.
package agentrail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"go.opentelemetry.io/otel/propagation"
)

const Version = "0.3.0"

type CollectorOptions struct {
	Endpoint string
	Secret   string
}
type QueueOptions struct {
	MaxRecords, BatchSize                                        int
	ScheduledDelay, ExportTimeout, FlushTimeout, ShutdownTimeout time.Duration
}
type LogsOptions struct {
	IncludeFields       []string
	ExcludeUncorrelated bool
}

// LocationSource identifies the trusted provider of an approximate IP location.
type LocationSource string

const (
	LocationDBIP       LocationSource = "dbip"
	LocationCloudflare LocationSource = "cloudflare"
	LocationVercel     LocationSource = "vercel"
	LocationConfigured LocationSource = "source"
)

// IPLocation describes a network exit, not a person's verified location.
// Country is an uppercase two-letter code. Empty place fields mean unknown.
// Values are copied at request admission; raw addresses are never included.
type IPLocation struct {
	Country, Region, City string
	Source                LocationSource
}

// MCPPayload is one MCP message the server received and its answer, as the
// MCP SDK encoded them. Request is the request's params; Response is the
// result, or the JSON-RPC error object. Name is the tool or prompt name, or the
// resource URI, where the method has one.
type MCPPayload struct {
	Method, Name      string
	Request, Response json.RawMessage
}

// MCPOptions configures capture by the mcp package's transport wrappers.
type MCPOptions struct {
	// CapturePayloads sends each request's params and its result as JSON text.
	// Off by default: without it no argument, result or resource content is sent.
	// It requires Redact.
	CapturePayloads bool
	// Redact, required with CapturePayloads, runs on every payload before it
	// is queued, on the goroutine that wrote the response. Return the payload to send, which must stay valid JSON,
	// or false to send none. It receives copies it may change. A panic, or JSON
	// that is not valid, drops the payload and is reported to OnError; the MCP
	// call is never affected.
	Redact func(MCPPayload) (MCPPayload, bool)
}

// Options configures backend capture. Secrets are supplied by the application;
// this package never reads environment variables implicitly.
type Options struct {
	Collector                               CollectorOptions
	HashKey, ServiceName, ServiceInstanceID string
	OutboundOrigins                         []string
	IgnoreIncomingRequest                   func(*http.Request) bool
	// IncomingLocation uses trusted platform metadata or a preloaded local database.
	// Return a zero value for unknown locations. Errors are reported without failing the request.
	IncomingLocation             func(*http.Request) (IPLocation, error)
	Logs                         LogsOptions
	Queue                        QueueOptions
	MCP                          MCPOptions
	Propagator                   propagation.TextMapPropagator
	OnError                      func(context.Context, CaptureError)
	endpointOrigin, endpointPath string
}

// CaptureError describes loss or refused context without including customer data.
type CaptureError struct {
	Code, Signal string
	Count        int
}

func (e CaptureError) Error() string {
	return fmt.Sprintf("Agentrail %s capture: %s (%d records)", e.Signal, e.Code, e.Count)
}

type ConfigurationError struct{ Problem string }

func (e *ConfigurationError) Error() string { return "Agentrail configuration: " + e.Problem }

var ErrClosed = errors.New("Agentrail capture is shut down")
var ErrUnbound = errors.New("Agentrail processors must be bound to a tracer provider")

type suppressionKey struct{}

func suppressed(ctx context.Context) bool { v, _ := ctx.Value(suppressionKey{}).(bool); return v }
func suppress(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressionKey{}, true)
}
func (o Options) report(ctx context.Context, code, signal string, count int) {
	// Diagnostics invoked with a suppressed context cannot recursively capture.
	e := CaptureError{Code: code, Signal: signal, Count: count}
	if o.OnError == nil {
		fmt.Fprintln(os.Stderr, e)
		return
	}
	defer func() {
		if recover() != nil {
			fmt.Fprintln(os.Stderr, "Agentrail onError callback panicked;", e)
		}
	}()
	o.OnError(ctx, e)
}
func validText(s string, max int) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0) && textLen(s) <= max
}
func textLen(s string) int { return len(utf16.Encode([]rune(s))) }
func bounded(s string, max int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "\ufffd"), "\x00", "")
	n := 0
	for i, r := range s {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if n+width > max {
			return s[:i]
		}
		n += width
	}
	return s
}
func origin(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port != "" && !(u.Scheme == "http" && port == "80") && !(u.Scheme == "https" && port == "443") {
		host += ":" + port
	}
	return strings.ToLower(u.Scheme) + "://" + host
}
func resolve(o Options) (Options, error) {
	bad := func(s string) (Options, error) { return o, &ConfigurationError{Problem: s} }
	u, err := url.Parse(o.Collector.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return bad("use an absolute HTTP(S) collector base URL without credentials, query or fragment")
	}
	if o.HashKey == "" || !utf8.ValidString(o.HashKey) || o.Collector.Secret == "" || strings.ContainsAny(o.Collector.Secret, " \t\r\n") || o.ServiceName == "" || !validText(o.ServiceName, 4096) || !validText(o.ServiceInstanceID, 4096) {
		return bad("supply a source secret, UTF-8 workspace hash key, and valid service identity")
	}
	o.Collector.Endpoint = strings.TrimRight(o.Collector.Endpoint, "/")
	o.endpointOrigin = origin(u)
	o.endpointPath = strings.TrimRight(u.EscapedPath(), "/")
	o.OutboundOrigins = append([]string(nil), o.OutboundOrigins...)
	o.Logs.IncludeFields = append([]string(nil), o.Logs.IncludeFields...)
	for _, s := range o.OutboundOrigins {
		u, e := url.Parse(s)
		if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || origin(u) != s || u.Path != "" {
			return bad("outbound origins must be exact canonical HTTP(S) origins")
		}
	}
	if o.MCP.CapturePayloads && o.MCP.Redact == nil {
		return bad("MCP payload capture needs a Redact hook")
	}
	q := &o.Queue
	if q.MaxRecords == 0 {
		q.MaxRecords = 2048
	}
	if q.BatchSize == 0 {
		q.BatchSize = 128
	}
	if q.ScheduledDelay == 0 {
		q.ScheduledDelay = time.Second
	}
	if q.ExportTimeout == 0 {
		q.ExportTimeout = 5 * time.Second
	}
	if q.FlushTimeout == 0 {
		q.FlushTimeout = 10 * time.Second
	}
	if q.ShutdownTimeout == 0 {
		q.ShutdownTimeout = 10 * time.Second
	}
	if q.MaxRecords < 1 || q.BatchSize < 1 || q.BatchSize > q.MaxRecords || q.BatchSize > 10000 || q.ScheduledDelay <= 0 || q.ExportTimeout <= 0 || q.FlushTimeout < q.ExportTimeout || q.ShutdownTimeout < q.ExportTimeout {
		return bad("positive queue limits, batch size <= capacity and 10000, and flush/shutdown deadlines >= export timeout are required")
	}
	return o, nil
}
