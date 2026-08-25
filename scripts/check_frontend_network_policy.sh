#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if ! command -v rg >/dev/null 2>&1; then
  echo "ripgrep is required to enforce the frontend network policy" >&2
  exit 1
fi
pattern='\bfetch\s*\(|\b(?:window|globalThis)\.fetch\b|\bXMLHttpRequest\b|\bEventSource\b|\bWebSocket\b|\bsendBeacon\s*\('
set +e
violations="$(rg -n --glob '*.{ts,tsx}' --glob '!**/*.test.*' --glob '!**/api/**' --glob '!**/platform/webrtc.ts' "$pattern" "$root/frontend/src" 2>&1)"
status=$?
set -e
if [[ $status -gt 1 ]]; then
  echo "frontend network policy scan failed:" >&2
  echo "$violations" >&2
  exit "$status"
fi
if [[ -n "$violations" ]]; then
  echo "Raw browser transports are restricted to frontend/src/api/** and platform/webrtc.ts:" >&2
  echo "$violations" >&2
  exit 1
fi
