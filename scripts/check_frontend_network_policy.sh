#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
violations="$({
  rg -n --glob '*.{ts,tsx}' --glob '!**/*.test.*' --glob '!**/api/**' --glob '!**/platform/webrtc.ts' '\bfetch\s*\(' "$root/frontend/src" || true
})"
if [[ -n "$violations" ]]; then
  echo "Raw fetch is restricted to frontend/src/api/** and platform/webrtc.ts:" >&2
  echo "$violations" >&2
  exit 1
fi
