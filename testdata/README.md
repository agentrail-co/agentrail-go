# Collector contract provenance

`sdk-contract.json` is copied byte for byte from Argus
`apps/api/sdk-contract.json` at commit
`ddb046d7fb5a884724e0f62fdf4441c64dee452c`
(includes MCP transport and handler spans, `otlp.mcpAttributes`,
`otlp.mcpSpans` (a 32-bit `jsonrpc.error_code`; resource URIs in span names
without userinfo, query or fragment), payload capture and `otlp.limits.mcpPayloadBytes` /
`exportBatchBytes`, standard MCP naming attributes and `tool_error`; SHA-256
`6a0b1e13d364da92831ba495b30a42ae0c1da7119c771e4b4a11a2e93743f498`).

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
