# Contributing to agentrail-go

Thanks for helping. Open an [issue](https://github.com/agentrail-co/agentrail-go/issues)
for a bug or a question; a minimal reproduction, the module version and the result
you expected help most. Pull requests are welcome. This repository is public: never
include a secret, a key or anything from a real account.

## Development

```sh
go build ./...
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go test ./internal/handoff -run '^$' -fuzz FuzzParseAndVerify -fuzztime 10s
scripts/check-consumer.sh
# Against a real Agentrail receiver (needs an Argus checkout, its dependencies and Docker):
ARGUS_DIR=/path/to/argus integration/run.sh
```

- How each part behaves, in detail: [docs/how-it-works.md](docs/how-it-works.md)
- Cross-language test vectors: [testdata](testdata/README.md)
- Receiver gate: [integration](integration/README.md)
- Verification evidence: [docs/verification.md](docs/verification.md)

A test must earn its place: say which realistic change it catches, and watch it
fail by making that change.

## Releasing

Go modules are served from Git tags, so there is no registry to publish to.
`Version` in `options.go` must match the tag: the release workflow checks it, runs
the full suite, the consumer gate and `govulncheck`, then creates the GitHub
release. Only repository admins can push a `v*` tag. A published version is
permanent in the Go module proxy, so a fix is a new version, never a moved tag.
Steps: [docs/releasing.md](docs/releasing.md).
