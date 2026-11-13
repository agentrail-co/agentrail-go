# Collector contract provenance

`sdk-contract.json` is copied byte for byte from Argus
`apps/api/sdk-contract.json` as generated on branch `feat/mcp-analytics` over
revision `23eaa10c2e85a5b86648468bc5e28e304394c19e`, before that change was
committed (adds MCP transport and handler spans, `otlp.mcpAttributes`,
`otlp.mcpSpans` and payload capture; SHA-256
`f8634c0c80a06de3b16f2568c9ec61093ff0fffcadbcb2f815ad8f7ae6e4e090`). Replace
this with the merged Argus revision once it lands.

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
