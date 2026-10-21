#!/usr/bin/env bash
# Run `go test` with an isolated HOME/XDG/TMPDIR so tests never read the user's
# config, skills or sessions, while keeping the real Go module and build caches.
#
#   scripts/gotest.sh ./internal/llm -run '^TestName$' -count=1
#   make test PKG=./internal/llm RUN='^TestName$' ARGS=-count=1
#
# GOTEST_RUN (set by `make test` from RUN) adds a literal -run pattern.
set -euo pipefail

run_args=()
if [[ -n "${GOTEST_RUN:-}" ]]; then
	run_args=(-run "$GOTEST_RUN")
fi
unset GOTEST_RUN

gomodcache="$(go env GOMODCACHE)"
gocache="$(go env GOCACHE)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-gotest.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home" "$tmp/config" "$tmp/data" "$tmp/cache" "$tmp/tmp"

HOME="$tmp/home" \
	XDG_CONFIG_HOME="$tmp/config" \
	XDG_DATA_HOME="$tmp/data" \
	XDG_CACHE_HOME="$tmp/cache" \
	TMPDIR="$tmp/tmp" \
	GOMODCACHE="$gomodcache" \
	GOCACHE="$gocache" \
	go test ${run_args[@]+"${run_args[@]}"} "$@"
