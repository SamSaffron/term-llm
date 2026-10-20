#!/usr/bin/env bash
# Builds the js/wasm platform smoke command and runs it under Node with a small
# JavaScript stack. The wasm binary stays in a temporary directory.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

node_bin=${NODE:-node}
if ! command -v "$node_bin" >/dev/null 2>&1; then
	echo "wasm smoke: node is required" >&2
	exit 1
fi

goroot=$(go env GOROOT)
wasm_exec="$goroot/lib/wasm/wasm_exec_node.js"
if [[ ! -f "$wasm_exec" ]]; then
	echo "wasm smoke: $wasm_exec is missing" >&2
	exit 1
fi

cd "$repo_root"
GOOS=js GOARCH=wasm go build -trimpath -o "$tmpdir/smoke.wasm" ./scripts/testdata/wasm
"$node_bin" --stack-size=64 "$wasm_exec" "$tmpdir/smoke.wasm"
