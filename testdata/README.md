# Collector contract provenance

`sdk-contract.json` is copied byte for byte from Argus
`apps/api/sdk-contract.json` at commit
`9625ffe6e2b60cd539a8fbd18445273d437c93fd`, branch
`feat/journey-sdk-request-facts`
(includes MCP transport and handler spans, `otlp.mcpAttributes`,
`otlp.mcpSpans` (a 32-bit `jsonrpc.error_code`; resource URIs in span names
without userinfo, query or fragment; a tool whose handler threw is a failed
call, with the thrown type, before `tool_error`), payload capture and
`otlp.limits.mcpPayloadBytes` / `exportBatchBytes`, standard MCP naming
attributes and `tool_error`, the HTTP request facts of
`otlp.mcpSpans.requestFacts` with `otlp.mcpRequestFacts`, the two recorded
client requests the MCP tests replay, and `otlp.headerNames`; SHA-256
`20adfff8a062274d104e9251e36834c3ef1b92821b03fc0a5d44ad86070afcba`).

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
