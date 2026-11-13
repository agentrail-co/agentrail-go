# Releasing agentrail-go

The module is prepared as version 0.3.0: MCP transport capture (`mcp.Handler`,
`mcp.Transport`), handler spans as children of transport spans, opt-in payload
capture (`Options.MCP`) and byte-sized span batches. v0.2.0 (2026-11-12) was
the first tag; v0.2.1 changed only the README. Tagging, pushing and
production deployment are separate operator steps; the build does not perform them.

1. The companion Argus recognition of `agentrail-go` (argus #547) is on
   production. Workflow and plugin evidence needs a collector that accepts the
   `agentrail.operation.*`, `agentrail.workflow.*` and `agentrail.plugin.*`
   attributes (argus #600 and later), also on production. MCP transport and
   handler spans, and payloads, need the Argus Agent tools change (branch
   `feat/mcp-analytics`, whose contract `testdata/sdk-contract.json` copies) on
   production before 0.3.0 is tagged; an older collector drops the new attributes. Existing OTLP endpoints must be enabled. No database migration
   or new source enum is needed; backend credentials currently have kind `node`.
2. Merge the SDK branch into the GitHub repository and run CI. Before tagging,
   run the real receiver gate against the deployed-code revision locally with
   disposable PostgreSQL. Confirm the compatibility matrix against the client.
3. Confirm `Version` in `options.go` is `0.3.0`, then create and push annotated tag
   `v0.3.0`. The release workflow verifies formatting/build/vet/race tests,
   vulnerability checks and a separate consumer module before creating the
   GitHub release. Enable GitHub Actions with workflow permission to write releases.
4. Go modules are distributed from version-control tags: there is no npm publish
   or package registry token. Verify `go list -m github.com/agentrail-co/agentrail-go@v0.3.0`
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
   receive signed context. Existing OTel users must compose sampler/processors
   during provider construction and bind the provider explicitly. Do not layer
   duplicate incoming HTTP tracing. Ensure Go 1.26+ and synchronized clocks.
   If instrumenting the collector server itself, explicitly exclude its ingestion
   routes using that server's `IgnoreIncomingRequest` policy.
8. Check a synthetic request/milestone/log in the receiver, inspect diagnostics,
   and wire graceful telemetry shutdown after request draining.

The module has independent versioning from agentrail-js. Before 1.0, minor version
changes may change APIs. Never include production credentials in release examples.
