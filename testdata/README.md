# Collector contract provenance

`sdk-contract.json` is copied byte for byte from Argus
`apps/api/sdk-contract.json` at commit
`7ec113b24b253d93f85633eb3e3caabd445e145a`, branch
`feat/journey-wire-row-library` (agentrail-co/argus#659)
(includes MCP transport and handler spans, `otlp.mcpAttributes`,
`otlp.mcpSpans` (a 32-bit `jsonrpc.error_code`; resource URIs in span names
without userinfo, query or fragment; a tool whose handler threw is a failed
call, with the thrown type, before `tool_error`), payload capture and
`otlp.limits.mcpPayloadBytes` / `exportBatchBytes`, standard MCP naming
attributes and `tool_error`, the HTTP request facts of
`otlp.mcpSpans.requestFacts` with `otlp.mcpRequestFacts`, the two recorded
client requests the MCP tests replay, `otlp.headerNames` (with `cdn-loop` and
`true-client-ip` among the names no caller writes), and the client id a
connected app's token was issued to (`principal.client`); SHA-256
`6f6a7b28c1a5249b8105f126e0bc62e97a0ee5e9cb38088ef56d3fab2524694a`).

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
