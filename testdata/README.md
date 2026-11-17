# Collector contract provenance

`sdk-contract.json` is copied byte for byte from Argus
`apps/api/sdk-contract.json` at commit
`92f514c7f`, branch `feat/cli-journeys` (agentrail-co/argus, the CLI journeys
change). It includes MCP transport and handler spans, `otlp.mcpAttributes`,
`otlp.mcpSpans`, payload capture and the `otlp.limits`, the HTTP request facts
of `otlp.mcpSpans.requestFacts` with `otlp.mcpRequestFacts`,
`otlp.headerNames`, and now `cliHeader` (the `x-agentrail-cli` header a
customer's CLI sends, how it is read, and its vectors), with
`otlp.cliAttributes` and `otlp.cliSpans`, which this SDK's middleware sends,
and the event's `cli` (SHA-256
`0d4cd14df4eb7cdcbc433ca62ea7b9b6364b696e73f02a8e31384a8062668454`).

Do not edit it in this repository. Update the receiver contract in Argus, copy
its generated snapshot and record the revision here. The SDK uses upstream OTLP
protobuf definitions; the contract's decoded examples are test evidence rather
than a custom transport. Handoff/marker subsets and reproducible JavaScript
interoperability vectors are in `interoperability/`; `operations.json` holds
operation-reference hashes computed by Node's `node:crypto` and
`JSON.stringify`, including agentrail-node's own test vector. Regenerate all of
them with `node testdata/interoperability/generate.mjs` beside an agentrail-js
checkout.

The Go-specific receiver recognition patch accompanies the initial SDK release.
It adds `agentrail-go` beside `agentrail-node` for identity/milestone semantics
(argus #547; `agentrail-python` is added the same way);
no endpoint, credential type or schema changes are required.
