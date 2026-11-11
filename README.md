# Agentrail for Go

Backend journey evidence for Go: HTTP traces, application-verified hashed identity,
explicit milestones, structured logs, and MCP operations. Sends OTLP HTTP/protobuf
to Agentrail using the official OpenTelemetry exporters.

Requires **Go 1.26+**. Tested dependency family: OpenTelemetry 1.47.0, log HTTP
exporter 0.23.0, official MCP Go SDK 1.8.0. The module recommends toolchain 1.26.10.
MCP and `slog` are separate opt-in imports in this module.

## Before installing

The receiver must expose `/ingest/otlp/v1/traces` and `/ingest/otlp/v1/logs` and
recognize `agentrail-go` for SDK milestones and verified identity. The companion
Argus compatibility change must be deployed first; older receivers accept HTTP
spans but omit explicit milestones and verified identity. See [release steps](docs/releasing.md).

Create a backend ingestion source (currently named **Node** in source setup).
Use its source secret and the workspace hash key. Browser and edge source
credentials cannot send backend OTLP. Keep both secrets on the server.

After a release is tagged:

```sh
go get github.com/agentrail-co/agentrail-go@v0.1.1
```

## HTTP startup

Validate environment values before construction. See [the complete HTTP example](examples/http/main.go).

```go
capture, err := agentrail.New(ctx, agentrail.Options{
    Collector: agentrail.CollectorOptions{
        Endpoint: "https://api.example.com/ingest/otlp",
        Secret: sourceSecret,
    },
    HashKey: workspaceHashKey,
    ServiceName: "orders-api",
    OutboundOrigins: []string{"https://inventory.internal"},
    OnError: func(ctx context.Context, issue agentrail.CaptureError) {
        logger.ErrorContext(ctx, "Agentrail capture", "code", issue.Code,
            "signal", issue.Signal, "count", issue.Count)
    },
})
if err != nil { return err }
server := &http.Server{Addr: ":8080", Handler: capture.Middleware(mux)}
httpClient := &http.Client{Transport: capture.Transport(nil)}
```

`New` owns local tracer and logger providers; it registers no globals and performs
no process-wide HTTP patching. Install the middleware once and use the wrapped
HTTP client for participating outbound calls. Child OTel instrumentation must use
the same provider; use the existing-provider mode below when composing libraries.
`Transport` accepts a concrete `*http.Transport`, or nil for a shared standard network
transport. This keeps propagation next to the network rather than allowing a
nested tracing transport to re-add forbidden headers. Install other client
middleware outside it.

The middleware preserves response streaming and optional writer interfaces. It
records the admitted scope: installed after authentication, it does not claim to
capture earlier authentication or parsing. Replace existing incoming `otelhttp`
tracing for this scope; wrapping both creates duplicate HTTP entries. A Go
`ServeMux` route pattern is captured when available.

Valid signed handoffs continue their trace. Public or refused context starts a
fresh trace with a directional link to a valid caller trace. The signature binds
the workspace key ID, traceparent, method, exact encoded path/query and a
millisecond issue time valid for 60 seconds. Configure the same workspace key
across participating edge/backend services and keep clocks synchronized. Apply
path/query rewrites before signing. Forwarded headers and caller-declared IDs do
not establish trust. Managed requests strip baggage, tracestate and Sentry trace
headers; only exact allowed origins receive newly signed propagation. Redirects
are checked at each destination, independently of an overridden Host header.

Use `IgnoreIncomingRequest` for trusted internal callers or health endpoints.
Validate any shared secret before configuring a matching callback. Collector
exports are excluded. If this middleware also runs on a collector server, use
`IgnoreIncomingRequest` in that server's configuration to exclude its known
ingestion routes. The SDK does not infer collector ingress from a Host header,
port or path shared with an unrelated application.

## Approximate IP location

Set `Options.IncomingLocation` to a synchronous
`func(*http.Request) (agentrail.IPLocation, error)` that reads trusted platform
metadata or a local database loaded at startup. `IPLocation` contains `Country`,
`Region`, `City` and `Source`. Country is a two-letter uppercase code; empty place
fields mean unknown. Return `agentrail.IPLocation{}` for an unavailable location.
Region and city are shortened to 128 UTF-16 units; empty values are omitted.
Use `LocationDBIP`, `LocationCloudflare`, `LocationVercel` or
`LocationConfigured` as the source. An empty source defaults to `LocationConfigured`.

Resolve the original client address using your application's trusted-proxy
configuration, skip private and reserved addresses, and never trust an arbitrary
forwarded header. The SDK does not look up the exporter connection's IP or send
client addresses to a geolocation service. The hook must return promptly and be
safe for concurrent calls. Its value is captured before the handler runs and is
shared only with that request's entry spans and nested milestones. Supporting
spans and logs do not gain location attributes. The collector derives the
observation time from each entry span's start time.

Lookup errors, callback panics, invalid country codes and unsupported sources
report `location_failed` through `OnError`; the application response continues
without location. No hook means no location. Approximate locations describe a
network exit, which can belong to a VPN, proxy or hosted agent rather than its user.

## Identity and milestones

```go
// Only after the application has authenticated this identity:
err := capture.Identify(r.Context(), agentrail.VerifiedIdentity{
    Kind: agentrail.PrincipalAgentRegistration,
    ID: authenticatedAgentID,
    ActingUserID: authenticatedUserID,
})
if err != nil { return err }
err = capture.WithMilestone(r.Context(), "checkout.confirm",
    agentrail.MilestoneContext{
        Surface: agentrail.SurfaceCustom, Kind: "checkout.confirm",
    }, func(ctx context.Context) error { return confirmOrder(ctx) })
```

`Identify` hashes IDs before export. Supported principal kinds are user,
agent_registration, agent_instance, oauth_client and api_key. Repeating the same
assertion has no effect; a conflicting assertion records an auth milestone and
preserves the original principal. Shared request state is safe across child
goroutines. Spans already ended before authentication cannot acquire a later
identity assertion. Work must retain the provided context; unrelated contexts
never inherit identity. Identify outside a managed operation reports a capture
issue. Invalid configuration/identity inputs return an error.

`Milestone` records an instant step. `StartMilestone` returns a child context and
an end function taking the application error. `WithMilestone` scopes a callback,
returns its original error and rethrows its original panic. Names/kinds must be
stable non-sensitive labels of 1–256 UTF-16 units, matching the collector limits.
Invalid milestone input reports a capture issue and leaves application work
running. Standalone milestones create their own managed scope.

## Structured logs

```go
logger := slog.New(agentrailslog.NewHandler(capture, existingHandler))
logger.ErrorContext(r.Context(), "private message", "err", applicationError,
    "event.name", "checkout.failed")
capture.Log(r.Context(), agentrail.StructuredLog{
    SeverityNumber: 17, ErrorType: "PaymentDeclined", EventName: "checkout.failed",
})
```

Import the adapter as `agentrailslog "github.com/agentrail-co/agentrail-go/slog"`.
Use context-bearing logger calls for exact span correlation. The adapter
preserves the existing handler's output, groups, filtering and errors.
`LogValuer` attributes are evaluated only by the application handler and are
excluded from capture.

Default exports contain severity, error type, event name, stable per-record ID
and exact span correlation. Message text, arbitrary attributes, exception
messages, request bodies and raw identities stay out. Supporting span names are
neutralized. URL evidence retains only valid Agentrail `h` markers; oversized
optional URLs are omitted without losing the entry. Resource metadata is reduced
to service/component/hash-key identity, including for external OTel log records.
Other customer exporters retain their original input.

Message fields require `Logs: agentrail.LogsOptions{IncludeFields: []string{"message"}}`.
Only explicitly selected scalar fields are encoded, in allowlist order, as valid
JSON bounded to 4,096 UTF-16 units including escaping and keys. Grouped slog keys
use dot notation. Do not select credentials or sensitive customer content. Set
`ExcludeUncorrelated: true` to omit logs outside managed requests. Generic logs
never create journey proof. Choose non-sensitive service names, routes, operation
names and user agents, as those remain customer-controlled context.

## Existing OpenTelemetry

Construct processors before the customer's provider:

```go
processors, err := agentrail.NewProcessors(ctx, options)
if err != nil { return err }
provider := sdktrace.NewTracerProvider(
    sdktrace.WithSampler(processors.Sampler(customerSampler)),
    sdktrace.WithSpanProcessor(customerProcessor),
    sdktrace.WithSpanProcessor(processors.SpanProcessor),
)
capture, err := processors.Bind(provider)
if err != nil { return err }
// Add processors.LogProcessor when constructing the customer LoggerProvider.
// Register processors.Propagator in any retained propagation integration.
```

See [the compiling composition example](examples/existing-otel/main.go).
`Bind` is one-time and explicit; the factory never discovers/replaces a global
provider. Retain customer resource configuration and other instrumentation.
The sampler wrapper turns dropped managed spans into record-only spans while
preserving the customer's sampled flag. Sampled-only exporters keep their
sampling; exporters that emit record-only spans may observe additional records.
A processor alone cannot recover spans already dropped by a sampler. Managed
HTTP wrappers perform request-aware signature verification; a propagator alone
cannot verify the method or target. Unmanaged propagation delegates to the
configured base propagator. Agentrail shutdown never shuts down a customer provider.

## MCP

Import `github.com/agentrail-co/agentrail-go/mcp` and wrap public registration
callbacks with `Tool`, `ToolHandler`, `Resource`, or `Prompt`. See
[the MCP example](examples/mcp/main.go). Pass stable registration names, never
caller-supplied resource URIs. The original typed output, result, error and panic
are preserved. Tool `IsError` results mark failure without replacing the result.

Tests exercise official SDK 1.8.0 modern 2026-07-28 and legacy 2025-11-25 protocols
on memory/Streamable HTTP, and legacy 2024-11-05 SSE. Modern HTTP requires the
SDK's stateless server mode. Client/protocol metadata comes from the SDK's public
request methods; it is untrusted descriptive context. No cross-caller initialize
cache is kept. Tool arguments, outputs, prompt text and resource URIs are excluded.
Each operation becomes a nested MCP milestone or its own scope outside HTTP.

## Delivery and shutdown

Per signal defaults: 2,048 queued/in-flight records, 128 records per batch, a
one-second schedule, five-second export timeout and ten-second flush/shutdown
deadlines. `QueueOptions` overrides must be positive; batch size must fit capacity
and the collector's 10,000-record limit. Flush/shutdown deadlines must cover the
export timeout. One batch exports at a time per signal; official exporters retry
retryable responses within the export deadline. Permanent rejections and exhausted
retries are reported and dropped. Memory queues have no disk persistence.

Stop accepting application requests, wait for handlers, then call
`capture.Shutdown(ctx)`. Shutdown is terminal and idempotent. Calls to `Flush`
wait for attempts for records already accepted when the call starts; successful
completion does not guarantee delivery. Inspect `Diagnostics()` and `OnError`
for overflow, export failure, deadlines and rejected context. Diagnostics report
accepted/exported/dropped/pending records; dropped also includes refused enqueue
attempts. SDK issues never replace an application response.

`OnError` runs synchronously outside queue locks and must return promptly. It
receives a capture-suppressed context: pass that context to your logger to prevent
recursive telemetry. Callback panics are contained with a safe stderr
fallback. An application callback that blocks cannot be forcibly terminated.

## Development

```sh
go build ./...
go vet ./...
go test -race ./...
go test ./internal/handoff -run '^$' -fuzz FuzzParseAndVerify -fuzztime 10s
scripts/check-consumer.sh
# With a compatible private Argus checkout, installed dependencies and Docker:
ARGUS_DIR=/path/to/argus integration/run.sh
```

The receiver gate uses synthetic data and disposable PostgreSQL; it creates and
removes a temporary test file in the supplied Argus checkout. See
[integration/README.md](integration/README.md) and [contract provenance](testdata/README.md).

## License

Apache-2.0. See [LICENSE](LICENSE).

Evidence and remaining operator checks: [verification](docs/verification.md).
