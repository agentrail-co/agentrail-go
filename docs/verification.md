# Initial Go SDK verification

Verified locally on macOS arm64 with Go 1.26.10 and the minimum Go 1.26.0.
No production data, deployment, release tag, GitHub push or publication was used.

## SDK checks

- `go build ./...`
- `go vet ./...`
- `go mod tidy -diff`
- `test -z "$(gofmt -l .)"`
- `go test -race -count=1 -json ./...`
- `GOTOOLCHAIN=go1.26.0 go test -count=1 ./...`
- `go test ./internal/handoff -run '^$' -fuzz FuzzParseAndVerify -fuzztime 10s`
- `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...`
- `scripts/check-consumer.sh`
- `ARGUS_DIR=/path/to/compatible/argus integration/run.sh`

All commands passed. Vulnerability scanning reported no reachable vulnerabilities.
The consumer gate installs an unpublished module archive through a temporary file
proxy and tests all public package imports from another module without `replace`.
It does not establish public module-proxy availability; verify that after tagging.

Tests exercise signed cross-language handoffs, malicious/untrusted propagation,
redirects, streaming and upgraded response bodies, body read errors, provider
composition/sampling, concurrent identity, errors/panics, log privacy and source
timestamps, slog behavior, real MCP protocol exchanges, queue limits and terminal
shutdown. Queue stress tests also passed repeated race runs.

Mutation checks caught disabled MAC verification, changed signing JSON escaping,
excluded in-flight queue capacity, removed logger/MCP capture, changed identity
comparison, dropped record-only sampling, replaced application errors, omitted
outbound header cleaning and omitted configuration validation. Each mutation was
restored. Additional RED-to-GREEN regressions caught response status/timing,
diagnostic reentry, collector URL/ingress exemption, missing log event names and
standalone external links.

## Receiver checks

The compatibility worktree is based on Argus
`e0690df0ef5fdece67991016ed97d980ce489bad`, with Go component recognition and docs.
The original checkout was not modified by this work. The Go receiver runner's
transient tests were removed after execution.

Using pinned Node 24.22.2 and pnpm 11.20.0:

- API typecheck passed.
- API lint passed with zero errors and existing repository warnings.
- API unit suite passed: 104 files, 930 tests.
- Targeted OTLP normalization passed: 14 tests, after proving Go admission failed
  before the compatibility patch.
- Existing OTLP ingestion integration passed: 8 tests.
- Real Go-to-Argus/PostgreSQL/linker gate passed: one test, verifying four spans,
  two entries, two logs, hashed principal/acting-user identity, canonical event
  names, supporting ancestry, exact log attachment and retry deduplication.
- Public-docs typecheck and unit tests passed; the Go installation page remains
  held outside the published site until release.

## Remaining operator checks

GitHub-hosted CI, the public tag/proxy, deployment and the customer's actual
backend configuration are unverified until the operator completes release and
installation. See [releasing.md](releasing.md). The full monorepo gate was not run:
no root configuration changed.

Git mutations used: `git switch -c feat/backend-sdk` in the SDK checkout;
`git worktree add ... -b feat/go-sdk-compat` for the companion change; task-owned
`git add` and `git commit` in each. Other Git commands were read-only status,
branch/revision/log/remote inspection, diff checks and remote tag/head lookups.
No pull, rebase, merge, amend, force push or push was performed.

## 0.2.0 checks (workflow and plugin evidence, toolchain go1.26.11)

Run on macOS arm64 with Go 1.26.10 (local), go1.26.11 (module toolchain) and
the minimum Go 1.26.0: `gofmt`, `go mod tidy -diff`, `go build`, `go vet`,
`go test -race -count=1 ./...`, `GOTOOLCHAIN=go1.26.0 go test ./...`,
`scripts/check-consumer.sh` (now compiles a milestone with performance evidence
from another module) and `govulncheck` under go1.26.10, go1.26.11 and go1.27.4:
no vulnerabilities found. The real receiver gate passed against Argus `main`
b2030266e. Mutations caught: `encoding/json` replacing the JSON.stringify
encoder in the operation hash, a 32-character truncated digest, the raw
reference exported, the namespace left out of the hash, evidence copied to
children through request state, a plugin-only call given a workflow role, the
namespace UUID unchecked, the event-name length unchecked, invalid evidence
dropping the milestone, and invalid evidence not reported.

## 0.3.0 checks (MCP transport capture and payloads, toolchain go1.26.11)

Run on macOS arm64 with go1.26.11: `gofmt`, `go mod tidy -diff`, `go build`,
`go vet`, `go test -race -count=1 ./...` (MCP tests also 10 times under race),
the handoff fuzz target for 10 seconds, `GOTOOLCHAIN=go1.26.0 go test ./...`,
`scripts/check-consumer.sh` (now compiles `mcp.Handler`, `mcp.Transport` and
`MCPOptions` from another module) and `govulncheck` v1.9.0: no vulnerabilities
found. Tests read attribute names and limits from `testdata/sdk-contract.json`.
The real receiver gate was not run: the Argus contract was uncommitted.
Mutations caught: payloads exported with capture off, a redact hook's drop
ignored, an oversized result sent whole instead of truncated, the JSON-RPC
error code lost, an unknown-method request never given a transport span, a
handler span counted as a second step, an attribute the contract does not name,
the stateful Streamable HTTP session hash dropped, the payload budget not
enforced or never released, and span batches closed by record count alone.
