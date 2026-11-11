#!/usr/bin/env bash
# Exercise code mode in a hardened native binary, without JIT/executable-memory
# entitlements. Run on both Intel and Apple Silicon macOS; Linux is not a proxy.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
if [[ "$(uname -s)" != Darwin ]]; then
	echo "macOS code-mode smoke requires native macOS" >&2
	exit 1
fi
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

scripts/gotest.sh -c -o "$scratch/codemode.test" ./internal/codemode
codesign --force --sign - --options runtime "$scratch/codemode.test"
codesign --verify --strict "$scratch/codemode.test"
codesign -dv "$scratch/codemode.test" 2>&1

mkdir -p "$scratch/home" "$scratch/config" "$scratch/data" "$scratch/cache" "$scratch/tmp"
HOME="$scratch/home" \
	XDG_CONFIG_HOME="$scratch/config" \
	XDG_DATA_HOME="$scratch/data" \
	XDG_CACHE_HOME="$scratch/cache" \
	TMPDIR="$scratch/tmp" \
	"$scratch/codemode.test" -test.v -test.timeout=5m
