# Real Argus receiver gate

Run from the Go SDK checkout with Go 1.26.10, Docker, and an Argus checkout with
its pinned Node 24/pnpm 11.20.0 dependencies installed:

```sh
ARGUS_DIR=/absolute/path/to/argus ./integration/run.sh
```

The runner builds and executes `capture/main.go` using the public Go SDK. A local
HTTP application identifies a synthetic user, opens a custom milestone, records
two correlated logs, and makes an outbound request beneath a supporting OTel
span. A local collector records the actual OTLP protobuf request bytes. There
are no provider calls, real identities, production configuration files, or
production credentials. The fixed collector token and workspace hash key in the
program are synthetic test constants.

The runner copies the receiver test into a uniquely named temporary Argus API
test file, then executes the normal `pnpm --filter @argus/api test:file` gate.
Argus's Testcontainers fixture creates a real PostgreSQL server and a private,
fully migrated database. The test creates a backend (`node`) journey source
through the public API and uses its temporary credential without printing it.
It sends the captured protobuf bytes with logs first and reversed trace batches,
then repeats every delivery unchanged.

Fresh database reads verify:

- Four immutable spans and two logs survive retries without duplication.
- The resource retains `agentrail-go`, its service identity, and hash key ID.
- Both the HTTP entry and custom milestone become entries; supporting spans
  remain ancestry beneath the milestone.
- Verified principal and acting-user IDs equal independently computed HMACs.
- The real linker persists one journey with an active trace proof, the intended
  custom milestone, exact supporting subtree, two correctly attached log
  summaries, one error, and no telemetry diagnostics.

Temporary captures and the copied Argus test are removed on exit, including
failures. The database fixture owns database/container cleanup. The runner does
not edit tracked Argus files or load an `.env` file. It prints the Argus revision
before the gate; local compatibility changes must also be reported alongside
that revision.

## Compatibility evidence

The initial run against unchanged Argus
`0f6aed4cc985bce2aa075aa5f10e540c9719af9b` reached the real receiver and database.
Every protobuf delivery returned 200, all four spans and two logs were persisted
exactly once, and resource/hash metadata and parent ancestry matched. The test
then failed because only the server span became an entry: the Go milestone was
not admitted. That revision recognizes `agentrail-node` alone for SDK milestone
and verified-identity semantics. This is a receiver compatibility requirement;
claiming the Go SDK is the Node SDK would conceal it.

The gate intentionally requires full Go semantics. A successful HTTP response
alone does not satisfy this test. Normal Go unit tests do not invoke Argus or
Docker; run this gate separately for receiver/linkage release evidence.

The location gate supplies a synthetic `IncomingLocation` result and verifies
city, region, country, provenance and observation time on both reconstructed
entries after repeated protobuf deliveries. It passed against Argus
`d97952be2d32bbe34ec22bd54d391da52a3f143b`. Removing location export made the
receiver assertion fail; restoring it passed. Older receivers without location
support cannot satisfy this gate.

The same gate passed with Node 24.22.2 against the isolated compatibility checkout
at `e0690df0ef5fdece67991016ed97d980ce489bad` plus the local receiver change that
recognizes `agentrail-go` alongside `agentrail-node`. Vitest reported one test
file and one test passed. This proves Go SDK → protobuf → receiver → PostgreSQL
→ linker persistence for this synthetic workload. An unchanged receiver with
the Node-only recognition remains incompatible with the full Go semantics.
