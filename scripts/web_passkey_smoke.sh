#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p tmp
state=$(mktemp -d "${PWD}/tmp/web-passkey-smoke.XXXXXX")
chmod 700 "$state"
server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  if [[ "${success:-false}" != true ]]; then if [[ -f "$state/server.log" ]]; then cat "$state/server.log" >&2; fi; echo "Artifacts: $state" >&2; else rm -rf "$state"; fi
}
trap cleanup EXIT
port=$(node -e 'const s=require("net").createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')
url="http://localhost:$port/ui/"
npm --prefix frontend run build
go build -o "$state/term-llm" .
mkdir -p "$state/config/term-llm"
printf 'default_provider: debug\nproviders:\n  debug:\n    model: fast\n' > "$state/config/term-llm/config.yaml"
if [[ -z "${PLAYWRIGHT_CHROMIUM_EXECUTABLE:-}" ]] && command -v chromium >/dev/null; then export PLAYWRIGHT_CHROMIUM_EXECUTABLE=$(command -v chromium); fi
for phase in enroll restart; do
  code="web-passkey-$phase-bootstrap-secret"
  bootstrap=""; recovery=""
  if [[ "$phase" == enroll ]]; then bootstrap="$code"; else recovery="$code"; fi
  env -u TERM_LLM_SERVE_TOKEN -u TERM_LLM_PPROF \
    HOME="$state" XDG_CONFIG_HOME="$state/config" XDG_DATA_HOME="$state/data" XDG_CACHE_HOME="$state/cache" \
    TERM_LLM_SERVE_BOOTSTRAP_TOKEN="$bootstrap" TERM_LLM_SERVE_RECOVERY_TOKEN="$recovery" \
    "$state/term-llm" serve web --auth passkey --public-url "$url" --port "$port" \
    --disable-widgets --disable-extensions --no-projects >"$state/server.log" 2>&1 &
  server_pid=$!
  for _ in $(seq 1 100); do
    if curl -fsS "${url}healthz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$server_pid" 2>/dev/null; then exit 1; fi
    sleep .1
  done
  curl -fsS "${url}healthz" >/dev/null
  if grep -Fq "$code" "$state/server.log"; then echo "Enrollment secret leaked to logs" >&2; exit 1; fi
  TERM_LLM_WEB_PASSKEY_URL="$url" TERM_LLM_WEB_PASSKEY_STATE="$state" \
    TERM_LLM_WEB_PASSKEY_PHASE="$phase" TERM_LLM_WEB_PASSKEY_CODE="$code" \
    npm --prefix frontend run test:e2e -- --project=desktop --workers=1 --output="$state/results" e2e/web-passkey.spec.ts
  kill "$server_pid"; wait "$server_pid"; server_pid=""
done
success=true
