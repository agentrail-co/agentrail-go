#!/usr/bin/env bash
# Real receiver gate: temporary test source + isolated migrated PostgreSQL fixture.
set -euo pipefail
sdk_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
argus_dir="${ARGUS_DIR:-${1:-$sdk_dir/../argus}}"
argus_dir="$(cd "$argus_dir" && pwd)"
if [[ ! -f "$argus_dir/apps/api/test/support/database.ts" ]]; then
  printf '%s\n' 'ARGUS_DIR must name an Argus checkout with installed dependencies.' >&2
  exit 2
fi
scratch="$(mktemp -d "${TMPDIR:-/tmp}/agentrail-go-gate.XXXXXXXX")"
owned_test="$argus_dir/apps/api/test/modules/agentrail-go-$(basename "$scratch").integration.test.ts"
test_created=0
cleanup() {
  if [[ "$test_created" == 1 ]]; then rm -f "$owned_test"; fi
  rm -rf "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$sdk_dir"
go run ./integration/capture --output "$scratch/capture.json"
# noclobber makes ownership explicit even in the unlikely event of a collision.
(set -o noclobber; cat integration/argus-receiver.integration.test.ts > "$owned_test")
test_created=1
cd "$argus_dir"
printf 'Argus receiver revision: '
git rev-parse HEAD
AGENTRAIL_GO_CAPTURE="$scratch/capture.json" NX_LOAD_DOT_ENV_FILES=false pnpm --filter @argus/api test:file "test/modules/$(basename "$owned_test")"
