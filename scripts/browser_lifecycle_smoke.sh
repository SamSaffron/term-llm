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
relay_binary="$(mktemp "${TMPDIR:-/tmp}/term-llm-webrtc-relay.XXXXXX")"
log="$(mktemp "${TMPDIR:-/tmp}/term-llm-browser-smoke.XXXXXX.log")"
relay_log="$(mktemp "${TMPDIR:-/tmp}/term-llm-webrtc-relay.XXXXXX.log")"
relay_cert="$(mktemp "${TMPDIR:-/tmp}/term-llm-webrtc-relay.XXXXXX.crt")"
relay_key="$(mktemp "${TMPDIR:-/tmp}/term-llm-webrtc-relay.XXXXXX.key")"
relay_ready="$(mktemp "${TMPDIR:-/tmp}/term-llm-webrtc-relay.XXXXXX.ready")"
rm -f "$relay_ready"
results="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-browser-results.XXXXXX")"
home="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-browser-home.XXXXXX")"
smoke_succeeded=false
cleanup() {
  if [[ -n "${relay_pid:-}" ]] && kill -0 "$relay_pid" 2>/dev/null; then
    kill -TERM "$relay_pid" 2>/dev/null || true
    wait "$relay_pid" 2>/dev/null || true
  fi
  if [[ -n "${server_pid:-}" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill -TERM "$server_pid" 2>/dev/null || true
    for _ in $(seq 1 40); do
      if ! kill -0 "$server_pid" 2>/dev/null; then break; fi
      sleep 0.1
    done
    if kill -0 "$server_pid" 2>/dev/null; then
      echo "Browser smoke server did not stop within 4 seconds; killing it" >&2
      kill -KILL "$server_pid" 2>/dev/null || true
    fi
    wait "$server_pid" 2>/dev/null || true
  fi
  if [[ "$smoke_succeeded" != true && -s "$log" ]]; then
    echo "Browser smoke server log:" >&2
    cat "$log" >&2
  fi
  if [[ "$smoke_succeeded" != true && -s "$relay_log" ]]; then
    echo "Browser smoke signaling relay log:" >&2
    cat "$relay_log" >&2
  fi
  rm -f "$binary" "$relay_binary" "$log" "$relay_log" "$relay_cert" "$relay_key" "$relay_ready"
  rm -rf "$results" "$home"
}
trap cleanup EXIT

cd "$root"
npm --prefix frontend run build
mkdir -p "$home/config/term-llm"
cat >"$home/config/term-llm/config.yaml" <<'YAML'
default_provider: debug
providers:
  debug:
    model: fast
YAML
go build -tags browserfixture -o "$binary" .
go build -o "$relay_binary" ./internal/testutil/webrtcrelay
"$relay_binary" --listen 127.0.0.1:0 --cert-out "$relay_cert" --key-out "$relay_key" --ready-out "$relay_ready" >"$relay_log" 2>&1 &
relay_pid=$!
for _ in $(seq 1 40); do
  if [[ -s "$relay_ready" ]]; then break; fi
  if ! kill -0 "$relay_pid" 2>/dev/null; then cat "$relay_log" >&2; exit 1; fi
  sleep 0.1
done
relay_url="$(cat "$relay_ready")"
curl -kfsS "$relay_url/healthz" >/dev/null

workspace="$home/workspace"
mkdir -p "$workspace"
printf 'original line\n' >"$workspace/review.txt"
git -C "$workspace" init -q
git -C "$workspace" -c user.name='Browser Fixture' -c user.email='fixture@example.invalid' add review.txt
git -C "$workspace" -c user.name='Browser Fixture' -c user.email='fixture@example.invalid' commit -qm 'fixture baseline'
(
  cd "$workspace"
  exec env -u TERM_LLM_PPROF -u TERM_LLM_SERVE_HUB_URL -u TERM_LLM_SERVE_HUB_REGISTER \
  -u TERM_LLM_SERVE_HUB_NODE_ID -u TERM_LLM_SERVE_HUB_NODE_NAME \
  HOME="$home" XDG_CONFIG_HOME="$home/config" XDG_DATA_HOME="$home/data" XDG_CACHE_HOME="$home/cache" \
  SSL_CERT_FILE="$relay_cert" TERM_LLM_WEBRTC_DIAGNOSTICS=1 \
  "$binary" serve web --no-auth --port "$port" --webrtc --enable-file-tracking \
  --webrtc-signaling-url "$relay_url" --webrtc-token browser-smoke \
  --webrtc-stun disabled --webrtc-diagnostics
) >"$log" 2>&1 &
server_pid=$!
for _ in $(seq 1 80); do
  if curl -fsS "${url}healthz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$server_pid" 2>/dev/null; then cat "$log" >&2; exit 1; fi
  sleep 0.25
done
curl -fsS "${url}healthz" >/dev/null

cd "$root/frontend"
if [[ -z "${PLAYWRIGHT_CHROMIUM_EXECUTABLE:-}" ]] && command -v chromium >/dev/null 2>&1; then
  export PLAYWRIGHT_CHROMIUM_EXECUTABLE="$(command -v chromium)"
fi
TERM_LLM_SMOKE_URL="$url" TERM_LLM_WEBRTC_RELAY_URL="$relay_url" npm run test:e2e -- --workers=1 --reporter=line --output="$results" "$@"
smoke_succeeded=true
