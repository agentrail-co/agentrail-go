# Agentrail for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/agentrail-co/agentrail-go.svg)](https://pkg.go.dev/github.com/agentrail-co/agentrail-go)
[![CI](https://github.com/agentrail-co/agentrail-go/actions/workflows/ci.yml/badge.svg)](https://github.com/agentrail-co/agentrail-go/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/agentrail-co/agentrail-go)](https://github.com/agentrail-co/agentrail-go/blob/main/go.mod)
[![Licence](https://img.shields.io/github/license/agentrail-co/agentrail-go)](https://github.com/agentrail-co/agentrail-go/blob/main/LICENSE)

`agentrail-go` adds your Go servers to the journeys [Agentrail](https://www.agentrail.co)
follows: the requests your server answers, the calls it makes, the identity your
application verified, the milestones you name, your `slog` logs and your MCP traffic.
It is built on OpenTelemetry, registers no globals, runs beside Sentry, Datadog and
your own OpenTelemetry with nothing in them to change, and never sends raw identities,
or bodies, arguments and results, unless you turn on MCP payload capture
(`Options.MCP.CapturePayloads`, with a required `Redact` hook).

## Requirements

- Go 1.26 or later
- A server source in Agentrail: open **Journeys**, select **Set up** and choose
  **Your servers**. Agentrail shows its **Secret** and **Hash key** once. Keep both
  on the server, for example as `AGENTRAIL_SECRET` and `AGENTRAIL_HASH_KEY`.

## Install

```sh
go get github.com/agentrail-co/agentrail-go@latest
```

## Quick start

```go
capture, err := agentrail.New(ctx, agentrail.Options{
	Collector: agentrail.CollectorOptions{
		Endpoint: "https://<collector>/ingest/otlp",
		Secret:   os.Getenv("AGENTRAIL_SECRET"),
	},
	HashKey:         os.Getenv("AGENTRAIL_HASH_KEY"),
	ServiceName:     "orders-api",
	OutboundOrigins: []string{"https://inventory.example.com"},
})
if err != nil {
	return err // an option is wrong
}

// Record incoming requests, and the outgoing calls made with this client.
server := &http.Server{Addr: ":8080", Handler: capture.Middleware(mux)}
inventory := &http.Client{Transport: capture.Transport(nil)}

// In a handler, once your application has authenticated the caller:
err = capture.Identify(r.Context(), agentrail.VerifiedIdentity{
	Kind: agentrail.PrincipalUser, ID: user.ID,
})

// Name the steps that matter. Your error and panic come back unchanged.
err = capture.WithMilestone(r.Context(), "checkout.confirm",
	agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "checkout.confirm"},
	func(ctx context.Context) error { return confirmOrder(ctx, inventory) })

// On shutdown: stop taking requests first, then send what is queued.
_ = server.Shutdown(shutdownCtx)
_ = capture.Shutdown(shutdownCtx)
```

For an MCP server, wrap its handler (or, for stdio, its transport) and its tools:

```go
mcpHandler := sdk.NewStreamableHTTPHandler(getServer, nil)
mux.Handle("/mcp", capturemcp.Handler(capture, mcpHandler))
// stdio: server.Connect(ctx, capturemcp.Transport(capture, &sdk.StdioTransport{}), nil)
sdk.AddTool(server, tool, capturemcp.Tool(capture, "confirm_order", confirmOrder))
```

Under `capturemcp.Handler`, each MCP span also says how the HTTP request that
carried its message was made, which is how Agentrail tells one agent product from
another: the method, the URL (its query cut to Agentrail's marker), the status,
the user agent, the header names (never a value; lower-cased and sorted, as Go
keeps neither their case nor their order), Accept, Accept-Encoding and the
JSON-RPC id. A message over stdio carries none of these.

When a customer's CLI uses [`agentrail-cli-sdk`](https://www.npmjs.com/package/agentrail-cli-sdk),
its requests say which command an agent ran, in which run and session. The
middleware records that on the request's span, the session only as a keyed hash,
so the agent's CLI runs join its journey. There is nothing to configure.

`New` never reads environment variables, so check yours are set before calling it.
Only the exact origins in `OutboundOrigins` receive Agentrail's trace context.
Complete programs: [HTTP server](https://github.com/agentrail-co/agentrail-go/blob/main/examples/http/main.go),
[MCP server](https://github.com/agentrail-co/agentrail-go/blob/main/examples/mcp/main.go),
[beside OpenTelemetry](https://github.com/agentrail-co/agentrail-go/blob/main/coexistence/examples/beside-otel/main.go).

## Beside Sentry, Datadog and OpenTelemetry

A server that already runs Sentry, Datadog or its own OpenTelemetry uses the same
setup and changes nothing in theirs. Agentrail keeps its own tracer and logger,
registers no OpenTelemetry global, and keeps its current span in a place of its own
in the request's context. The other tracer's spans stay where they were, its trace
headers still reach the services you call, and Agentrail records the same spans
with or without it. Put the other tracer's middleware and client transport outside
Agentrail's:

```go
server := &http.Server{Addr: ":8080", Handler: otelhttp.NewHandler(capture.Middleware(mux), "orders-api")}
inventory := &http.Client{Transport: otelhttp.NewTransport(capture.Transport(nil))}
```

Sentry's `sentryhttp` handler and `sentryhttpclient` round tripper, and Datadog's
`httptrace.WrapHandler` and `httptrace.WrapRoundTripper`, go in the same places.
`Transport` takes a concrete `*http.Transport` (or nil), so Agentrail's headers are
set next to the network.

On calls to any origin outside `OutboundOrigins`, the other tracer's headers
(`traceparent`, `tracestate`, `baggage`, `sentry-trace`, `x-datadog-*`) go out as it
set them, and Agentrail's own never do. On a call to an allowed origin, Agentrail's
signed `traceparent` replaces the other tracer's and its `tracestate` is dropped;
its other headers go through unchanged. One limit follows for a tracer that
propagates by `traceparent` alone, as OpenTelemetry does by default: its trace for
that one hop continues under Agentrail's trace ID. Sentry (`sentry-trace`) and
Datadog (`x-datadog-*`) keep their own trace there, as their headers go through.

Agentrail records only its own spans. Spans your code or its libraries start inside
a request go to your tracer, not to Agentrail, and `trace.SpanFromContext` in a
handler returns your tracer's span, never Agentrail's.

### Upgrading from 0.4

0.5.0 has one setup, `New`, for every server. Removed: `NewProcessors`, the
`Processors` type (`SpanProcessor`, `LogProcessor`, `Propagator`, `Bind`,
`Sampler`), `Options.Propagator` and `ErrUnbound`. Replace them with `New` and keep
your provider as it was. Your own spans and OpenTelemetry log records are no longer
copied to Agentrail, and calls made while answering a request now keep your trace
headers.

## Documentation

| Topic | Guide |
| --- | --- |
| Set up, identity, milestones, logs, MCP, location | [Install the Go SDK](https://www.agentrail.co/docs/guides/journeys-go) |
| Every option, Sentry, Datadog and OpenTelemetry, delivery and error codes | [Go SDK reference](https://www.agentrail.co/docs/guides/journeys-go-reference) |
| Leaving out health checks and your own services | [Leave out your own servers](https://www.agentrail.co/docs/guides/journeys-own-servers) |
| Workflow attempts and plugin calls | [Plugin workflow tracking](https://www.agentrail.co/docs/guides/plugin-performance) |
| What is sent, and what never is | [What Agentrail records](https://www.agentrail.co/docs/guides/journeys-data) |
| Package API | [pkg.go.dev](https://pkg.go.dev/github.com/agentrail-co/agentrail-go) |

The same SDK for other languages: [Node](https://github.com/agentrail-co/agentrail-js/tree/main/packages/node)
and [Python](https://github.com/agentrail-co/agentrail-python). Before 1.0, a minor
version may change the API.

## Contributing

Bugs and questions: [GitHub issues](https://github.com/agentrail-co/agentrail-go/issues).
Building, testing and releasing: [CONTRIBUTING.md](https://github.com/agentrail-co/agentrail-go/blob/main/CONTRIBUTING.md).

## Licence

[Apache-2.0](https://github.com/agentrail-co/agentrail-go/blob/main/LICENSE).
