#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ ! -x "$root/frontend/node_modules/.bin/playwright" ]]; then
  echo "frontend smoke dependencies are missing; run 'cd frontend && npm ci' first" >&2
  exit 1
fi
if ! command -v npm >/dev/null 2>&1; then
  echo "npm is required to run the frontend browser smoke" >&2
  exit 1
fi
if [[ -n "${TERM_LLM_SMOKE_PORT:-}" ]]; then
  port="$TERM_LLM_SMOKE_PORT"
else
  port="$(python3 - <<'PY'
import socket
with socket.socket() as server:
    server.bind(('127.0.0.1', 0))
    print(server.getsockname()[1])
PY
)"
fi
url="http://127.0.0.1:${port}/ui/"
binary="$(mktemp "${TMPDIR:-/tmp}/term-llm-browser-smoke.XXXXXX")"
log="$(mktemp "${TMPDIR:-/tmp}/term-llm-browser-smoke.XXXXXX.log")"
results="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-browser-results.XXXXXX")"
smoke_succeeded=false
cleanup() {
  if [[ -n "${server_pid:-}" ]]; then kill "$server_pid" 2>/dev/null || true; fi
  if [[ "$smoke_succeeded" != true && -s "$log" ]]; then
    echo "Browser smoke server log:" >&2
    cat "$log" >&2
  fi
  rm -f "$binary" "$log"
  rm -rf "$results"
}
trap cleanup EXIT

cd "$root"
go build -o "$binary" .
"$binary" serve web --no-auth --port "$port" >"$log" 2>&1 &
server_pid=$!
for _ in $(seq 1 80); do
  if curl -fsS "${url}healthz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$server_pid" 2>/dev/null; then cat "$log" >&2; exit 1; fi
  sleep 0.25
done
curl -fsS "${url}healthz" >/dev/null

cd "$root/frontend"
TERM_LLM_SMOKE_URL="$url" npm run test:e2e -- --workers=1 --reporter=line --output="$results"
smoke_succeeded=true
