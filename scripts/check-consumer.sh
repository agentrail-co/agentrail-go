#!/usr/bin/env bash
# Prove a separate consumer installs the unpublished module without replace.
set -euo pipefail
sdk_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/agentrail-go-consumer.XXXXXXXX")"
cleanup() {
  chmod -R u+w "$scratch"
  rm -rf "$scratch"
}
trap cleanup EXIT
python3 - "$sdk_dir" "$scratch" <<'PY'
import json, pathlib, re, sys, zipfile
root, scratch = map(pathlib.Path, sys.argv[1:])
module = 'github.com/agentrail-co/agentrail-go'
version = 'v' + re.search(r'const Version = "([^"]+)"', (root/'options.go').read_text()).group(1)
base = scratch/'proxy'/module/'@v'
base.mkdir(parents=True)
(base/f'{version}.mod').write_bytes((root/'go.mod').read_bytes())
(base/f'{version}.info').write_text(json.dumps({'Version':version,'Time':'2026-11-09T00:00:00Z'}))
(base/'list').write_text(version+'\n')
with zipfile.ZipFile(base/f'{version}.zip','w',zipfile.ZIP_DEFLATED) as z:
    for p in root.rglob('*'):
        rel=p.relative_to(root)
        if any(x in {'.git','.build','.github'} for x in rel.parts) or not p.is_file(): continue
        z.write(p, f'{module}@{version}/{rel.as_posix()}')
(scratch/'version').write_text(version)
PY
mkdir "$scratch/consumer"
cd "$scratch/consumer"
export GOPROXY="file://$scratch/proxy,https://proxy.golang.org"
export GONOSUMDB="github.com/agentrail-co/agentrail-go"
# Isolate the SDK's module cache entry from any public version with the same tag.
export GOMODCACHE="$scratch/modcache"
go mod init example.com/agentrail-consumer
go mod edit -go=1.26.0 -toolchain=go1.26.10
go get "github.com/agentrail-co/agentrail-go@$(cat "$scratch/version")"
cat > main_test.go <<'GO'
package consumer
import (
 "context"
 "net/http"
 "testing"
 agentrail "github.com/agentrail-co/agentrail-go"
 capturemcp "github.com/agentrail-co/agentrail-go/mcp"
 captureslog "github.com/agentrail-co/agentrail-go/slog"
 "log/slog"
 sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)
func TestPublicInstallation(t *testing.T){
 c,err:=agentrail.New(context.Background(),agentrail.Options{Collector:agentrail.CollectorOptions{Endpoint:"http://127.0.0.1:1/ingest/otlp",Secret:"synthetic-source"},HashKey:"synthetic-key",ServiceName:"consumer"});if err!=nil{t.Fatal(err)}
 _ = c.Middleware(http.HandlerFunc(func(http.ResponseWriter,*http.Request){}))
 _ = c.Transport(nil)
 _ = captureslog.NewHandler(c,slog.Default().Handler())
 _ = capturemcp.ToolHandler(c,"operation",func(context.Context,*sdk.CallToolRequest)(*sdk.CallToolResult,error){return &sdk.CallToolResult{},nil})
 if err:=c.Shutdown(context.Background());err!=nil{t.Fatal(err)}
}
GO
go mod tidy
go test -race ./...
if rg '^replace' go.mod; then
  printf '%s\n' 'consumer unexpectedly uses replace' >&2
  exit 1
fi
printf '%s\n' 'Consumer installed and tested without replace directives.'
