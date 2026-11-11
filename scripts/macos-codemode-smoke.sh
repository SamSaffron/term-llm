#!/usr/bin/env bash
# Native Apple Silicon regression for the release entitlement. Ad-hoc signatures
# test hardened compiler execution, not Developer ID trust or notarization.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
if [[ "$(uname -s)/$(uname -m)" != Darwin/arm64 ]]; then
	echo "This regression requires native Apple Silicon macOS" >&2
	exit 1
fi
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
ulimit -c 0
scripts/gotest.sh -c -o "$scratch/tools.test" ./internal/tools
mkdir -p "$scratch/home" "$scratch/config" "$scratch/data" "$scratch/cache" "$scratch/tmp"
export HOME="$scratch/home" XDG_CONFIG_HOME="$scratch/config" XDG_DATA_HOME="$scratch/data"
export XDG_CACHE_HOME="$scratch/cache" TMPDIR="$scratch/tmp"

# These children may be killed by code-signing enforcement. Never execute the
# compiler in the coordinating process. allow-jit only covers MAP_JIT, which
# wazero v1.11.0 does not use; verify that it is insufficient here too.
sed 's/allow-unsigned-executable-memory/allow-jit/' scripts/macos-entitlements.plist > "$scratch/jit.plist"
for variant in none jit release; do
	binary="$scratch/$variant.test"
	cp "$scratch/tools.test" "$binary"
	set -- --force --sign - --options runtime
	case "$variant" in
		jit) set -- "$@" --entitlements "$scratch/jit.plist" ;;
		release) set -- "$@" --entitlements scripts/macos-entitlements.plist ;;
	esac
	codesign "$@" "$binary"
	codesign --verify --strict "$binary"
	codesign -dv "$binary" 2>&1
	codesign -d --entitlements :- "$binary"
	set +e
	"$binary" -test.v -test.run '^TestCodeModeHello$' -test.timeout=2m > "$scratch/$variant.log" 2>&1
	status=$?
	set -e
	cat "$scratch/$variant.log"
	echo "$variant: exit $status"
	if [[ "$variant" == release ]]; then
		[[ "$status" == 0 ]] || exit 1
		grep -q -- '--- PASS: TestCodeModeHello' "$scratch/$variant.log"
	else
		# SIGKILL (not an arbitrary test failure) proves hardened enforcement.
		[[ "$status" == 137 ]] || exit 1
		grep -q '=== RUN   TestCodeModeHello' "$scratch/$variant.log"
	fi
done
