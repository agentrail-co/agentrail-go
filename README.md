# Agentrail for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/agentrail-co/agentrail-go.svg)](https://pkg.go.dev/github.com/agentrail-co/agentrail-go)
[![CI](https://github.com/agentrail-co/agentrail-go/actions/workflows/ci.yml/badge.svg)](https://github.com/agentrail-co/agentrail-go/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/agentrail-co/agentrail-go)](https://github.com/agentrail-co/agentrail-go/blob/main/go.mod)
[![Licence](https://img.shields.io/github/license/agentrail-co/agentrail-go)](https://github.com/agentrail-co/agentrail-go/blob/main/LICENSE)

`agentrail-go` adds your Go servers to the journeys [Agentrail](https://www.agentrail.co)
follows: the requests your server answers, the calls it makes, the identity your
application verified, the milestones you name, your `slog` logs and your MCP tool calls.
It is built on OpenTelemetry, registers no globals, and never sends bodies,
arguments, results or raw identities.

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

`New` never reads environment variables, so check yours are set before calling it.
Only the exact origins in `OutboundOrigins` receive Agentrail's trace context.
Complete programs: [HTTP server](https://github.com/agentrail-co/agentrail-go/blob/main/examples/http/main.go),
[MCP server](https://github.com/agentrail-co/agentrail-go/blob/main/examples/mcp/main.go),
[existing OpenTelemetry](https://github.com/agentrail-co/agentrail-go/blob/main/examples/existing-otel/main.go).

## Documentation

| Topic | Guide |
| --- | --- |
| Set up, identity, milestones, logs, MCP, location | [Install the Go SDK](https://www.agentrail.co/docs/guides/journeys-go) |
| Every option, an existing OpenTelemetry setup, delivery and error codes | [Go SDK reference](https://www.agentrail.co/docs/guides/journeys-go-reference) |
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
