# Collector contract provenance

`sdk-contract.json` is copied byte for byte from Argus
`apps/api/sdk-contract.json` at revision
`d97952be2d32bbe34ec22bd54d391da52a3f143b`.

Do not edit it in this repository. Update the receiver contract in Argus, copy
its generated snapshot and record the revision here. The SDK uses upstream OTLP
protobuf definitions; the contract's decoded examples are test evidence rather
than a custom transport. Handoff/marker subsets and reproducible JavaScript
interoperability vectors are in `interoperability/`.

The Go-specific receiver recognition patch accompanies the initial SDK release.
It adds `agentrail-go` beside `agentrail-node` for identity/milestone semantics;
no endpoint, credential type or schema changes are required.
