# Agentrail for Go

`agentrail-go` adds your Go servers to the journeys Agentrail follows. When an
AI agent or a person reaches your product, the steps they take on your backend —
HTTP requests, MCP tool calls, the identity your application verified and the
business milestones you name — are recorded and sent to Agentrail, which links
them with what your edge and pages saw into one journey.

It is built on OpenTelemetry's official Go SDK and exporters (OTLP over
HTTP/protobuf). It registers no global providers and patches nothing
process-wide: you add one middleware and wrap the HTTP clients you choose.

- **Go 1.26 or later.** Tested with OpenTelemetry 1.47.0 (log exporter 0.23.0)
  and the official MCP Go SDK 1.8.0.
- **Private by default.** Request and response bodies, tool arguments and
  results, log messages and raw identities are never sent. See
  [What is sent](#what-is-sent).
- **Your application never fails because of it.** A capture problem goes to
  your `OnError` callback, never to the caller.

## Contents

- [Install](#install)
- [Quickstart](#quickstart)
- [Verified identity](#verified-identity)
- [Milestones](#milestones)
- [Workflow and plugin evidence](#workflow-and-plugin-evidence)
- [Structured logs with slog](#structured-logs-with-slog)
- [MCP servers](#mcp-servers)
- [Calls from your own servers](#calls-from-your-own-servers)
- [Approximate IP location](#approximate-ip-location)
- [An existing OpenTelemetry setup](#an-existing-opentelemetry-setup)
- [Delivery, diagnostics and shutdown](#delivery-diagnostics-and-shutdown)
- [What is sent](#what-is-sent)
- [Options](#options)
- [Node, Go and Python](#node-go-and-python)
- [Development](#development) · [Releasing](#releasing) · [Licence](#licence)

## Install

```sh
go get github.com/agentrail-co/agentrail-go@v0.2.0
```

You need two values from your Agentrail workspace, and both stay on your
servers — never put them in a page or a client app:

- the **source secret** of a backend source. Backend sources are named
  **Node server** in Agentrail; the Go, Node and Python SDKs all use them.
  The setup screens create browser and edge sources today; ask Agentrail for
  a backend source until they offer one.
- the workspace **hash key**, which every SDK in the workspace shares so the
  same person or operation hashes to the same value everywhere.

## Quickstart

```go
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	capture, err := agentrail.New(ctx, agentrail.Options{
		Collector: agentrail.CollectorOptions{
			Endpoint: os.Getenv("AGENTRAIL_ENDPOINT"), // https://<collector>/ingest/otlp
			Secret:   os.Getenv("AGENTRAIL_SOURCE_SECRET"),
		},
		HashKey:         os.Getenv("AGENTRAIL_HASH_KEY"),
		ServiceName:     "orders-api",
		OutboundOrigins: []string{"https://inventory.example.com"},
		OnError: func(ctx context.Context, issue agentrail.CaptureError) {
			slog.ErrorContext(ctx, "agentrail capture", "code", issue.Code,
				"signal", issue.Signal, "count", issue.Count)
		},
	})
	if err != nil {
		slog.Error("agentrail", "error", err) // invalid options: fix the configuration
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkout", checkout)

	// Incoming requests: one middleware around the scope you want recorded.
	server := &http.Server{Addr: ":8080", Handler: capture.Middleware(mux),
		ReadHeaderTimeout: 5 * time.Second}

	// Outgoing requests: wrap the clients whose calls belong to the journey.
	inventory := &http.Client{Transport: capture.Transport(nil)}
	_ = inventory

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "error", err)
		}
	}()
	<-ctx.Done()

	// Stop taking requests first, then flush what was captured.
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	_ = capture.Shutdown(shutdown)
}

func checkout(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) }
```

`New` validates its options and returns a `*ConfigurationError` when one is
wrong; it never reads environment variables itself. Validate your own
environment before calling it.

- **`Middleware`** records each incoming request, including the `ServeMux`
  route pattern, and keeps streaming and optional writer interfaces working.
  Install it once. If you already trace incoming requests with `otelhttp`,
  replace that for this scope: wrapping both records every request twice.
  Installed after authentication, it records only what runs after it.
- **`Transport`** records outgoing calls. Pass your `*http.Transport`, or `nil`
  for a shared standard one, and install other client middleware outside it.
  Only the exact origins in `OutboundOrigins` receive Agentrail's signed trace
  context; every other destination receives none, and redirects are checked at
  each hop.
- **Signed handoffs.** When `agentrail-edge` or another Agentrail SDK forwards
  a request with a valid signature, the backend continues that trace. Anything
  else — a public caller's `traceparent`, an expired or forged signature —
  starts a fresh trace with a link to the caller's. Configure the same
  workspace hash key everywhere, and keep clocks in sync: a signature is valid
  for 60 seconds.

Complete, compiling examples: [HTTP server](examples/http/main.go),
[MCP server](examples/mcp/main.go),
[existing OpenTelemetry](examples/existing-otel/main.go).

## Verified identity

After your application has authenticated the caller, say who it is:

```go
err := capture.Identify(r.Context(), agentrail.VerifiedIdentity{
	Kind:         agentrail.PrincipalAgentRegistration,
	ID:           agent.ID,
	ActingUserID: agent.UserID, // optional
})
```

IDs are hashed with the workspace hash key before they leave the process.
Kinds: `PrincipalUser`, `PrincipalAgentRegistration`, `PrincipalAgentInstance`,
`PrincipalOAuthClient`, `PrincipalAPIKey`.

- Only `Identify` sets identity. Headers, OpenTelemetry `user.id` attributes
  and what an MCP client says about itself are never trusted as identity.
- Repeating the same identity does nothing. A different one records an
  `auth.identity` milestone and keeps the first.
- Identity belongs to the request's context: goroutines that carry it share it,
  other requests never do. Spans that ended before `Identify` stay anonymous.
- `Identify` returns an error for an invalid kind or ID. Called outside a
  recorded request, it reports `identity_outside_request` to `OnError`.

## Milestones

Name the business steps that matter, with stable labels — never IDs or
arguments:

```go
// An instant step:
capture.Milestone(ctx, "cart.viewed",
	agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "cart.viewed"})

// A scoped step: returns your error unchanged, and re-panics your panic.
err := capture.WithMilestone(ctx, "checkout.confirm",
	agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "checkout.confirm"},
	func(ctx context.Context) error { return confirmOrder(ctx) })

// Or start and end it yourself:
ctx, end := capture.StartMilestone(ctx, "refund.issue",
	agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "refund.issue"})
err = issueRefund(ctx)
end(err)
```

Names and kinds are 1–256 characters. A milestone outside a recorded request
starts its own. Invalid input reports `invalid_milestone` and your code keeps
running.

## Workflow and plugin evidence

To track real workflow attempts — for example a delivery requested through a
ChatGPT or Claude plugin and completed by your backend — attach an operation
reference after your application has authorized the operation:

```go
capture.Milestone(ctx, "delivery.requested", agentrail.MilestoneContext{
	Surface: agentrail.SurfaceCustom,
	Kind:    "business.event",
	Performance: &agentrail.PerformanceContext{
		Environment: agentrail.EnvironmentProduction,
		Operation: &agentrail.AuthorizedOperation{
			NamespaceID: operationNamespaceID, // from Agentrail
			InstanceRef: delivery.AttemptID,   // hashed locally, never sent
			Role:        agentrail.RoleStart,
		},
		EventName: "delivery.requested",
		Plugin: &agentrail.PluginInvocation{
			BindingID:    pluginBindingID, // from Agentrail
			InvocationID: invocationID,    // opaque, for example a random UUID
		},
	},
})
```

- Use a new `InstanceRef` for each business attempt, and the same one for its
  start, progress, plugin calls and outcome — across services, which must share
  the workspace hash key and namespace.
- Send `RoleTerminal` with your configured success event **only after your
  backend has verified the result**. A tool returning, or an HTTP 200, is not
  completion; the SDK never infers it.
- A plugin call with no authorized operation sends only `Environment` and
  `Plugin`: it shows as plugin activity without workflow credit.
- Production and test are kept apart by `Environment`.
- The reference is sent only as an HMAC-SHA256 of
  `["agentrail.operation", namespaceId, instanceRef]` keyed by the hash key —
  the same value the Node and Python SDKs produce. Never put raw references in
  names, invocation IDs or log fields.
- Evidence the collector would refuse is dropped from that milestone and
  reported as `invalid_performance`; the milestone and your code still run.
  The exact rules are in [docs/how-it-works.md](docs/how-it-works.md#workflow-and-plugin-evidence).
- Evidence applies to that milestone only; nothing inside it inherits it.

The collector accepts this evidence from sources Agentrail has granted the
namespace's permissions.

## Structured logs with slog

```go
import agentrailslog "github.com/agentrail-co/agentrail-go/slog"

logger := slog.New(agentrailslog.NewHandler(capture, existingHandler))
logger.ErrorContext(r.Context(), "payment declined", "event.name", "checkout.failed")

// Or record one directly:
capture.Log(r.Context(), agentrail.StructuredLog{
	SeverityNumber: 17, ErrorType: "PaymentDeclined", EventName: "checkout.failed",
})
```

The handler passes every record to your handler unchanged. Agentrail receives
the severity, error type, event name, a stable record ID and the exact span it
belongs to — use the `…Context` logger calls so it can tell. Message text and
attributes are not sent unless you list them:

```go
Logs: agentrail.LogsOptions{
	IncludeFields:       []string{"message", "order.status"}, // grouped keys use dots
	ExcludeUncorrelated: true, // drop logs outside recorded requests
},
```

Selected fields must be scalars, and are sent as JSON of at most 4,096
characters. Never select credentials or customer content. Logs never link
journeys on their own; they add detail to steps that are already linked.

## MCP servers

Wrap the handlers you register with the official MCP Go SDK:

```go
import (
	capturemcp "github.com/agentrail-co/agentrail-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

sdk.AddTool(server, &sdk.Tool{Name: "confirm_order"},
	capturemcp.Tool(capture, "confirm_order", confirmOrder))
// capturemcp.ToolHandler, capturemcp.Resource and capturemcp.Prompt wrap the others.
```

Each call becomes an MCP step nested in its HTTP request (or its own step
outside one), with the registered name, method, client name and version, and
protocol. Arguments, results, prompt text and resource URIs are never sent;
pass a stable registered name, not the caller's URI. Your result, error and
panic are returned unchanged, and a tool result with `IsError` marks the step
failed. Mount the MCP handler behind `capture.Middleware` and your
authentication. Tested with SDK 1.8.0 on the 2026-07-28 protocol (stateless
Streamable HTTP), 2025-11-25 and 2024-11-05 (SSE).

## Calls from your own servers

Requests your own services send — health checks, internal jobs — are not
visitors or agents. Leave them out, and check something only your servers can
send, such as a shared secret. A user agent or an address is what any caller
can claim:

```go
internalSecret := os.Getenv("INTERNAL_SECRET")
if internalSecret == "" {
	return errors.New("INTERNAL_SECRET is not set")
}
options.IgnoreIncomingRequest = func(r *http.Request) bool {
	return subtle.ConstantTimeCompare(
		[]byte(r.Header.Get("X-My-Internal")), []byte(internalSecret)) == 1
}
```

Check the secret is set first: left empty, a request without the header would
match, and nothing would be recorded. The SDK's own calls to the collector are
always excluded.

## Approximate IP location

```go
// Only behind Cloudflare, which sets CF-IPCountry and strips a caller's copy.
options.IncomingLocation = func(r *http.Request) (agentrail.IPLocation, error) {
	country := r.Header.Get("CF-IPCountry")
	if country == "" || country == "XX" || country == "T1" { // unknown, Tor
		return agentrail.IPLocation{}, nil
	}
	return agentrail.IPLocation{Country: country, Source: agentrail.LocationCloudflare}, nil
}
```

The hook runs for each recorded request. Read trusted platform metadata or a
local database loaded at startup — no network calls — and never an arbitrary
forwarded header. `Country` is a two-letter uppercase code; `Region` and `City`
are optional; `Source` is `LocationCloudflare`, `LocationVercel`,
`LocationDBIP` or `LocationConfigured`. Return `agentrail.IPLocation{}` when
there is no location. An error or an invalid value reports `location_failed`
and the request continues without one. The address itself is never sent, and a
location describes a network exit — possibly a VPN or a hosted agent.

## An existing OpenTelemetry setup

If your service already builds its own tracer provider, add Agentrail's parts
while you construct it:

```go
processors, err := agentrail.NewProcessors(ctx, options)
if err != nil {
	return err
}
provider := sdktrace.NewTracerProvider(
	sdktrace.WithSampler(processors.Sampler(yourSampler)),
	sdktrace.WithSpanProcessor(yourProcessor),
	sdktrace.WithSpanProcessor(processors.SpanProcessor),
)
capture, err := processors.Bind(provider)
// Add processors.LogProcessor to your LoggerProvider, and use
// processors.Propagator wherever you register a propagator.
```

The sampler keeps the spans Agentrail needs as record-only, without changing
what your sampler decided or what your exporters receive. Your providers stay
yours: `capture.Shutdown` never shuts them down. See the
[compiling example](examples/existing-otel/main.go).

## Delivery, diagnostics and shutdown

Each signal (traces, logs) has a bounded in-memory queue: 2,048 records, sent
in batches of 128 every second, with a 5-second export timeout and 10 seconds
to flush or shut down. Change them with `Options.Queue`. Retryable collector
responses are retried within the export timeout; nothing is written to disk.

- `capture.Flush(ctx)` waits for the records accepted so far to be attempted.
- `capture.Shutdown(ctx)` flushes once and stops; call it after your server
  has stopped taking requests. It is final, and safe to call twice.
- `capture.Diagnostics()` reports accepted, exported, dropped and pending
  records per signal.
- `OnError` receives each problem — overflow, export failure, a refused
  handoff, invalid input — with a code and count, never customer data. It
  runs synchronously, so return quickly. Log with the context it gives you:
  that context is not captured again.

## What is sent

| Sent | Never sent |
| --- | --- |
| Method, route pattern, status, timing, span relationships | Request and response bodies, headers, cookies |
| URL path, with only Agentrail's own `h` markers kept in the query | Other query parameters |
| HMAC hashes of verified identities and operation references | Raw IDs, emails, operation references |
| MCP operation name, method, client name/version, protocol | Tool arguments, results, prompt text, resource URIs |
| Log severity, error type, event name, record ID | Log messages and attributes, unless you list them |
| Service name and instance, SDK name and version, hash key ID | The hash key, the source secret |
| Country, region, city when you supply them | IP addresses |

Service names, route patterns, milestone names and user agents come from your
application: keep them free of personal data.

## Options

| Field | Required | Meaning |
| --- | --- | --- |
| `Collector.Endpoint` | yes | The collector's OTLP base URL, ending `/ingest/otlp` |
| `Collector.Secret` | yes | The backend source secret |
| `HashKey` | yes | The workspace hash key |
| `ServiceName` | yes | A stable, non-sensitive service name |
| `ServiceInstanceID` | no | This instance, when you run several |
| `OutboundOrigins` | no | Exact origins (`https://host[:port]`) that receive signed trace context |
| `IgnoreIncomingRequest` | no | Leave a request out ([your own servers](#calls-from-your-own-servers)) |
| `IncomingLocation` | no | Approximate location ([IP location](#approximate-ip-location)) |
| `Logs` | no | `IncludeFields`, `ExcludeUncorrelated` |
| `Queue` | no | Queue size, batch size and timeouts |
| `Propagator` | no | Your existing propagator, used for work outside Agentrail's requests |
| `OnError` | no | Capture problems; defaults to standard error |

## Node, Go and Python

The three backend SDKs offer the same capture, send the same evidence and
interoperate: a signed handoff from one continues in another, and they hash
identities and operations identically.

| | Node — [`agentrail-node`](https://github.com/agentrail-co/agentrail-js/tree/main/packages/node) | Go — `agentrail-go` | Python — [`agentrail-python`](https://github.com/agentrail-co/agentrail-python) |
| --- | --- | --- | --- |
| Incoming HTTP | `node:http` (any framework on it) | `net/http` middleware | ASGI and WSGI middleware |
| Outgoing HTTP | `http`, `fetch` | `http.RoundTripper` | `httpx`, `requests` |
| Signed handoffs | ✓ | ✓ | ✓ |
| Verified identity | ✓ | ✓ | ✓ |
| Milestones | ✓ | ✓ | ✓ |
| Workflow and plugin evidence | ✓ | ✓ | ✓ |
| Structured logs | Pino, OpenTelemetry logs | `slog`, OpenTelemetry logs | `logging`, OpenTelemetry logs |
| MCP | `@modelcontextprotocol/server` | official Go SDK | official `mcp` SDK |
| Existing OpenTelemetry | ✓ | ✓ | ✓ |
| IP location, own-server exclusion | ✓ | ✓ | ✓ |
| Runtime | Node 22+ | Go 1.26+ | Python 3.10+ |

The browser script and the edge recorder are JavaScript only:
[`agentrail-browser`, `agentrail-edge`](https://github.com/agentrail-co/agentrail-js).

## Development

```sh
go build ./...
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go test ./internal/handoff -run '^$' -fuzz FuzzParseAndVerify -fuzztime 10s
scripts/check-consumer.sh
# Against a real Agentrail receiver (needs an Argus checkout, its dependencies and Docker):
ARGUS_DIR=/path/to/argus integration/run.sh
```

How each part behaves, in detail: [docs/how-it-works.md](docs/how-it-works.md).
Cross-language test vectors: [testdata](testdata/README.md). Receiver gate:
[integration](integration/README.md).

## Releasing

Go modules are served from Git tags, so there is no registry to publish to.
`Version` in `options.go` must match the tag: the release workflow checks it,
runs the full suite, the consumer gate and `govulncheck`, then creates the
GitHub release. Steps: [docs/releasing.md](docs/releasing.md). Before 1.0, a
minor version may change the API.

## Licence

[Apache-2.0](LICENSE).
