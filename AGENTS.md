# Agentrail Go SDK

Public Go module for backend journey evidence. Reference behavior lives in
`agentrail-co/agentrail-js` packages/node; collector schema belongs to Argus.

- Read the implementation and README before changing behavior.
- Run `go build ./...`, `go vet ./...`, `go test -race ./...` and
  `scripts/check-consumer.sh` before reporting completion.
- Contract snapshots in `testdata/` are copied from Argus, never edited here.
- Add meaningful behavioral tests; prove a realistic regression makes them fail.
- Never retain unsanitized spans/logs in delivery queues or modify customer
  exporter input. Never export bodies, arguments, results or raw identity by default.
- Managed requests continue only verified signed handoffs. Actual destination,
  exact method and encoded target determine outbound admission.
- No implicit global providers. Customer-owned providers remain customer-owned.
- Keep bounded queue capacity inclusive of in-flight records, preserve context
  cancellation, application errors/panics and HTTP streaming interfaces.
- No production credentials or customer evidence in this public repository.
- Feature work uses a branch; no rebase, force push or amend of pushed commits.
- Release tags must match `Version` in options.go. Publishing is a separate action.
