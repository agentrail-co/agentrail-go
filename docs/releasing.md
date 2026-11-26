# Releasing agentrail-go

The module is prepared as version 0.5.0: one setup, `New`, which runs beside
Sentry, Datadog and the application's own OpenTelemetry. Agentrail's current span
lives under a context key of its own, so the application's OpenTelemetry span is
never cleared, and calls keep the application's trace headers (an allowed
origin's `traceparent` is replaced by Agentrail's signed one and its `tracestate`
dropped). `NewProcessors`, `Processors` (`SpanProcessor`, `LogProcessor`,
`Propagator`, `Bind`, `Sampler`), `Options.Propagator` and `ErrUnbound` are
removed, and the application's own spans and log records are no longer copied.
For a server with no other tracer, what 0.5.0 sends is unchanged; beside one,
Agentrail's spans and logs no longer take that tracer's span as their parent or
correlation. 0.4.0 made the middleware read a customer CLI's `x-agentrail-cli`
header (written by `agentrail-cli-sdk`) into `agentrail.cli.*` span attributes,
the session only as its keyed hash. 0.3.0 added MCP transport
capture (`mcp.Handler`, `mcp.Transport`), handler spans as children of transport
spans, opt-in payload capture (`Options.MCP`), byte-sized span batches and the
HTTP request facts on MCP transport spans. v0.2.0 (2026-11-12) was
the first tag; v0.2.1 changed only the README. Tagging, pushing and
production deployment are separate operator steps; the build does not perform them.

1. 0.5.0 needs no collector change. The public Go guides describe the removed
   existing-OpenTelemetry setup until the Argus docs change for 0.5.0 is live;
   publish it with the tag. 0.4.0 needed the Argus CLI journeys change (branch
   `feat/cli-journeys`, commit
   `92f514c7f`; `testdata/README.md` names the commit `testdata/sdk-contract.json`
   copies now) on production before
   it is tagged: an older collector drops the `agentrail.cli.*` attributes and
   keeps the rest of the span, so nothing breaks, but no CLI run joins a journey
   until it is live. The companion Argus recognition of `agentrail-go` (argus #547) is on
   production. Workflow and plugin evidence needs a collector that accepts the
   `agentrail.operation.*`, `agentrail.workflow.*` and `agentrail.plugin.*`
   attributes (argus #600 and later), also on production. MCP transport and
   handler spans, and payloads, need the Argus Agent tools change (branch
   `feat/mcp-analytics`) on production before 0.3.0 is tagged, and so does the
   collector that ranks a thrown handler above `IsError` (Argus commit
   `78f9621d4`, branch `fix/release-621-prod-check`); an older collector drops
   the new attributes. `testdata/sdk-contract.json` copies Argus commit
   `7ec113b24` (agentrail-co/argus#659, in the stack from #653 to #661), whose
   collector reads the MCP request facts. A collector without it ignores those attributes and
   keeps the rest of the span, so it need not deploy first, but no journey reads
   header facts from a Go MCP span until it is on production. Existing OTLP endpoints must be enabled. No database migration
   or new source enum is needed; backend credentials currently have kind `node`.
2. Merge the SDK branch into the GitHub repository and run CI. Before tagging,
   run the real receiver gate against the deployed-code revision locally with
   disposable PostgreSQL. Confirm the compatibility matrix against the client.
3. Confirm `Version` in `options.go` is `0.5.0`, then create and push annotated tag
   `v0.5.0`. The release workflow verifies formatting/build/vet/race tests,
   vulnerability checks and a separate consumer module before creating the
   GitHub release. Enable GitHub Actions with workflow permission to write releases.
4. Go modules are distributed from version-control tags: there is no npm publish
   or package registry token. Verify `go list -m github.com/agentrail-co/agentrail-go@v0.5.0`
   and `go get` from a fresh project after the public tag exists. The local
   consumer gate verifies an unpublished module through a temporary file proxy;
   it does not prove public proxy availability or a published tag.
5. Publish the held Go installation/privacy documentation after the SDK tag and
   compatible receiver are live. Source setup still calls the backend source
   **Node**; explain this until product terminology is updated.
6. Configure the customer's backend with a backend source secret, the workspace
   hash key, OTLP base endpoint and non-sensitive service name. Prefer explicit
   secret-manager/environment loading in the application. No public/browser keys.
7. Install one HTTP middleware, wrapped network transports for outbound calls,
   and request-context logging. Configure only exact outbound origins that should
   receive signed context. A server that runs Sentry, Datadog or OpenTelemetry
   keeps that setup unchanged, with their middleware and transport outside
   Agentrail's. Ensure Go 1.26+ and synchronized clocks.
   If instrumenting the collector server itself, explicitly exclude its ingestion
   routes using that server's `IgnoreIncomingRequest` policy.
8. Check a synthetic request/milestone/log in the receiver, inspect diagnostics,
   and wire graceful telemetry shutdown after request draining.

The module has independent versioning from agentrail-js. Before 1.0, minor version
changes may change APIs. Never include production credentials in release examples.
