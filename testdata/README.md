# Collector contract provenance

`sdk-contract.json` is copied byte for byte from Argus
`apps/api/sdk-contract.json` at commit
`78f9621d4673eab0f3e454575b98903352ee02f6`
(includes MCP transport and handler spans, `otlp.mcpAttributes`,
`otlp.mcpSpans` (a 32-bit `jsonrpc.error_code`; resource URIs in span names
without userinfo, query or fragment; a tool whose handler threw is a failed
call, with the thrown type, before `tool_error`), payload capture and
`otlp.limits.mcpPayloadBytes` / `exportBatchBytes`, standard MCP naming
attributes and `tool_error`; SHA-256
`a25284880b58df53e19d2c2854cbe1bc66defb6349f7c9b96009c975ac628a19`).

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
