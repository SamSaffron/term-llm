#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# git grep ships with every checkout and CI runner, so the scan needs no extra
# package install. --untracked keeps new, not-yet-added sources in scope.
pattern='\bfetch\s*\(|\b(?:window|globalThis)\.fetch\b|\bXMLHttpRequest\b|\bEventSource\b|\bWebSocket\b|\bsendBeacon\s*\('
set +e
violations="$(git -C "$root" grep --untracked -nP "$pattern" -- \
  ':(glob)frontend/src/**/*.ts' ':(glob)frontend/src/**/*.tsx' \
  ':(exclude,glob)**/*.test.*' ':(exclude,glob)frontend/src/**/api/**' 2>&1)"
status=$?
set -e
if [[ $status -gt 1 ]]; then
  echo "frontend network policy scan failed:" >&2
  echo "$violations" >&2
  exit "$status"
fi
if [[ -n "$violations" ]]; then
  echo "Raw browser transports are restricted to frontend/src/api/**:" >&2
  echo "$violations" >&2
  exit 1
fi
