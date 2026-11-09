// Copyright 2026 Agentrail. Licensed under the Apache License, Version 2.0.
package agentrail

import (
	"context"
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

const Version = "0.1.0"

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

// Options configures backend capture. Secrets are supplied by the application;
// this package never reads environment variables implicitly.
type Options struct {
	Collector                               CollectorOptions
	HashKey, ServiceName, ServiceInstanceID string
	OutboundOrigins                         []string
	IgnoreIncomingRequest                   func(*http.Request) bool
	Logs                                    LogsOptions
	Queue                                   QueueOptions
	Propagator                              propagation.TextMapPropagator
	OnError                                 func(context.Context, CaptureError)
	endpointOrigin, endpointPath            string
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
