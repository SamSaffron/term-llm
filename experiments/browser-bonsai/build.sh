#!/usr/bin/env bash
# Build the static browser demo:
#   1. real Vite frontend assets (make frontend; cached once built)
#   2. the Go/WASM agent entrypoint + matching wasm_exec.js
#   3. the parent-owned JS worker/boot/inference bundle
#   4. the genuine serve-ui shell with boot.mjs wired before the app entry
#
# Output: experiments/browser-bonsai/dist (serve with cmd/serve).
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$root"

out="$root/experiments/browser-bonsai/dist"
static="$root/internal/serveui/static"

if [[ ! -f "$static/dist/asset-manifest.json" && ! -f "$static/dist/asset-manifest.json.gz" ]]; then
  echo "==> building frontend assets (once)"
  make frontend
fi

echo "==> building Go/WASM entrypoint"
mkdir -p "$out"
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o "$out/term-llm.wasm" ./experiments/browser-bonsai
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$out/wasm_exec.js"

if [[ ! -f "$root/experiments/browser-bonsai/web/runtime/python-root.tar.gz" || ! -f "$root/experiments/browser-bonsai/web/python-runtime.mjs" ]]; then
  echo "==> fetching pinned CPython runtime"
  "$root/experiments/browser-bonsai/scripts/fetch-python-runtime.sh"
fi

echo "==> bundling browser inference runtime"
(cd experiments/browser-bonsai && node bundle.mjs "$out")

echo "==> rendering the real web shell"
go run ./experiments/browser-bonsai/cmd/shell -out "$out" -static "$static"

echo
echo "static demo: $out"
ls -lh "$out" | sed -n '1,20p'
