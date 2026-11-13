# How agentrail-go works

The detail behind the [README](../README.md): what each part of the SDK
guarantees, and what it deliberately does not do. Read the README first.

## HTTP capture and signed handoffs

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

Import `github.com/agentrail-co/agentrail-go/mcp`. See
[the MCP example](../examples/mcp/main.go).

**Transport spans.** `Handler(capture, h)` wraps an `sdk.StreamableHTTPHandler`
or `sdk.SSEHandler`; mount it inside `capture.Middleware`. `Transport(capture, t)`
wraps a transport the application passes to `Server.Connect` (stdio, in-memory,
`IOTransport`). Nothing is patched: without a wrapper there are no transport
spans. Each JSON-RPC request or notification the server receives becomes one
span and one step (`agentrail.step.surface` `mcp`; kind `tool.call` for
`tools/call`, `mcp.<method>` otherwise), including `initialize`, `tools/list`,
`notifications/initialized` and requests the SDK refuses before any middleware,
such as an unknown method. Names are `mcp.<method>`, or `mcp.<method>:<target>`
where the target is the tool or prompt name, or the resource URI, the client
sent, bounded to 256 UTF-16 units. Attributes are `agentrail.mcp.layer`
`transport`, `agentrail.mcp.method` (left off when it does not match the
collector's pattern), client name, version and protocol, the keyed hash of the
`Mcp-Session-Id` (the same HMAC as principal IDs; absent for stdio, in-memory,
SSE and stateless sessions), the encoded size in bytes of the request and of
its response (as `jsonrpc.EncodeMessage` writes them; no response size for a
notification), `agentrail.mcp.jsonrpc.error_code` for a JSON-RPC error and
`agentrail.mcp.result.is_error` for a failed tool result. Both set status ERROR,
with `error.type` `McpJsonRpcError` or `McpOperationError`. A request the HTTP
layer refuses without a JSON-RPC answer (a plain-text 4xx in older protocols)
is ERROR with no code. The span ends when the response is written, before the
client can read it.

`Transport` gives each request an empty `*sdk.RequestExtra` when its transport
supplies none, which is how a callback finds its transport span. Like
`sdk.LoggingTransport`, it hides the SDK's private connection hooks, so a stdio
session no longer refuses JSON-RPC batches in protocol 2025-06-18 and later.
`Handler` reads each POST body (up to 64 MiB; a larger one passes through
unrecorded) and the responses and event streams the SDK writes, preserving
streaming and the writer's optional interfaces. It remembers the client
metadata of the 4,096 most recent stateful sessions.

**Handler spans.** Wrap registration callbacks with `Tool`, `ToolHandler`,
`Resource` or `Prompt`. Pass stable registration names, which name the
callback's span. Inside a transport span the callback's span is its child,
`agentrail.mcp.layer` `handler`, and no step, so a tool call counts once. With
no transport wrapper it is the call's step, as before, with `layer` `handler`
and `agentrail.mcp.method`. The original typed output, result, error and panic
are preserved; tool `IsError` results mark failure without replacing the result.
Client metadata is untrusted descriptive context, never identity.

**Payloads, opt-in.** By default no argument, result, prompt text or resource
content is sent. Set `Options.MCP.CapturePayloads` to send each request's params
as `agentrail.mcp.request.payload` and its result, or JSON-RPC error object, as
`agentrail.mcp.response.payload`, as JSON text. `Options.MCP.Redact` receives an
`MCPPayload{Method, Name, Request, Response}` copy and returns what to send, or
false to send none. A panic or invalid JSON drops that payload and reports
`payload_redact_failed` or `payload_invalid`; the MCP call is unaffected.
Payloads have no length limit of their own and are exempt from the string caps
elsewhere; only a span whose payloads could not fit one 16 MiB export is cut, as
valid UTF-8, with `agentrail.mcp.request.truncated` or `.response.truncated`
set. At most 64 MiB of payload text is held per span queue, the batch in flight
included; past it a span is sent without its payloads and
`payload_budget_exceeded` is reported. The collector keeps payloads only for a
source whose Admin turned payload capture on.

Tests exercise official SDK 1.8.0 modern 2026-07-28 and legacy 2025-11-25 protocols
on memory and Streamable HTTP (stateful and stateless), and legacy 2024-11-05 SSE.
Modern HTTP requires the SDK's stateless server mode.

## Delivery and shutdown

Per signal defaults: 2,048 queued/in-flight records, 128 records per batch, a
one-second schedule, five-second export timeout and ten-second flush/shutdown
deadlines. A span batch also closes before its estimated size would pass 15 MiB,
under the collector's 16 MiB body limit. `QueueOptions` overrides must be positive; batch size must fit capacity
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

## Workflow and plugin evidence

`MilestoneContext.Performance` is checked when the milestone starts. Evidence
the collector would refuse is left off the span and reported to `OnError` as
`invalid_performance`; the milestone and the application's callback still run.
The rules match the collector's contract:

- `Environment` is `EnvironmentProduction` or `EnvironmentTest`.
- An `Operation` needs an `EventName`, a `Plugin`, or both. A `Plugin` alone
  must not carry an `EventName`, and earns no workflow-outcome credit.
- `NamespaceID` and `BindingID` are lowercase canonical UUIDs (version 1–8,
  RFC 4122 variant).
- `InstanceRef` is nonempty valid UTF-8. It is hashed with HMAC-SHA256, keyed
  by the workspace hash key, over the UTF-8 JSON array
  `["agentrail.operation", namespaceId, instanceRef]` encoded as JavaScript's
  `JSON.stringify` does. The full 64-character hex digest is exported; the
  reference itself never is. `testdata/interoperability/operations.json` holds
  vectors produced by Node, including the Node SDK's own test vector.
- `Role` is `RoleStart`, `RoleProgress` or `RoleTerminal`.
- `EventName` is 1–120 ASCII letters, digits, `.`, `_`, `:` or `-`, starting
  with a letter.
- `InvocationID` is 1–256 UTF-16 units of valid UTF-8 without NUL.

Evidence is attached to the milestone's own span only. Child milestones,
outbound calls and logs inside it never inherit it.
