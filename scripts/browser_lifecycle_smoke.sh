#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -n "${TERM_LLM_SMOKE_PORT:-}" ]]; then
  port="$TERM_LLM_SMOKE_PORT"
else
  port="$(node -e 'const net = require("node:net"); const server = net.createServer(); server.listen(0, "127.0.0.1", () => { console.log(server.address().port); server.close(); });')"
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

cd "$root/internal/serveui"
TERM_LLM_SMOKE_URL="$url" npx --yes --package @playwright/test@1.54.1 sh -c \
  'export NODE_PATH="$(dirname "$(dirname "$(command -v playwright)")")"; playwright test browser_lifecycle.spec.js --workers=1 --reporter=line --output="'"$results"'"'
smoke_succeeded=true
