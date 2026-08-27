#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ ! -x "$root/frontend/node_modules/.bin/playwright" ]]; then
  echo "frontend smoke dependencies are missing; run 'cd frontend && npm ci' first" >&2
  exit 1
fi
free_port() {
  python3 - <<'PY'
import socket
with socket.socket() as server:
    server.bind(('127.0.0.1', 0))
    print(server.getsockname()[1])
PY
}
node_port="$(free_port)"
hub_port="$(free_port)"
node_token="production-node-bearer"
hub_token="production-hub-bearer"
hub_root="http://127.0.0.1:${hub_port}/hub/"
node_root="http://127.0.0.1:${node_port}/chat/"
binary="$(mktemp "${TMPDIR:-/tmp}/term-llm-hub-browser.XXXXXX")"
relay_binary="$(mktemp "${TMPDIR:-/tmp}/term-llm-hub-relay.XXXXXX")"
state="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-hub-browser-state.XXXXXX")"
results="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-hub-browser-results.XXXXXX")"
relay_ready="$state/relay.ready"
succeeded=false
cleanup() {
  for pid in "${hub_pid:-}" "${node_pid:-}" "${relay_pid:-}"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [[ "$succeeded" != true ]]; then
    for log in "$state/hub.log" "$state/node.log" "$state/relay.log"; do
      if [[ -s "$log" ]]; then echo "== $log ==" >&2; cat "$log" >&2; fi
    done
  fi
  rm -f "$binary" "$relay_binary"
  rm -rf "$state" "$results"
}
trap cleanup EXIT

cd "$root"
npm --prefix frontend run build
go build -tags browserfixture -o "$binary" .
go build -o "$relay_binary" ./internal/testutil/webrtcrelay
"$relay_binary" --listen 127.0.0.1:0 --cert-out "$state/relay.crt" --key-out "$state/relay.key" --ready-out "$relay_ready" >"$state/relay.log" 2>&1 &
relay_pid=$!
for _ in $(seq 1 80); do
  [[ -s "$relay_ready" ]] && break
  kill -0 "$relay_pid" 2>/dev/null || { cat "$state/relay.log" >&2; exit 1; }
  sleep .1
done
relay_url="$(cat "$relay_ready")"
curl -kfsS "$relay_url/healthz" >/dev/null

home="$state/home"
workspace="$state/workspace"
mkdir -p "$home/config/term-llm" "$workspace"
cat >"$home/config/term-llm/config.yaml" <<'YAML'
default_provider: debug
providers:
  debug:
    model: fast
YAML
printf 'production hub fixture\n' >"$workspace/fixture.txt"
git -C "$workspace" init -q
git -C "$workspace" -c user.name='Hub Browser Fixture' -c user.email='fixture@example.invalid' add fixture.txt
git -C "$workspace" -c user.name='Hub Browser Fixture' -c user.email='fixture@example.invalid' commit -qm fixture
(
  cd "$workspace"
  exec env -u TERM_LLM_PPROF -u TERM_LLM_SERVE_HUB_URL -u TERM_LLM_SERVE_HUB_REGISTER \
    -u TERM_LLM_SERVE_HUB_NODE_ID -u TERM_LLM_SERVE_HUB_NODE_NAME \
    HOME="$home" XDG_CONFIG_HOME="$home/config" XDG_DATA_HOME="$home/data" XDG_CACHE_HOME="$home/cache" \
    SSL_CERT_FILE="$state/relay.crt" TERM_LLM_WEBRTC_DIAGNOSTICS=1 \
    "$binary" serve web --host 127.0.0.1 --port "$node_port" --base-path /chat \
    --token "$node_token" --webrtc --webrtc-signaling-url "$relay_url" \
    --webrtc-token production-hub-smoke --webrtc-stun disabled --webrtc-diagnostics
) >"$state/node.log" 2>&1 &
node_pid=$!
for _ in $(seq 1 80); do
  if curl -fsS -H "Authorization: Bearer $node_token" "${node_root}healthz" >/dev/null 2>&1; then break; fi
  kill -0 "$node_pid" 2>/dev/null || { cat "$state/node.log" >&2; exit 1; }
  sleep .25
done
curl -fsS -H "Authorization: Bearer $node_token" "${node_root}healthz" >/dev/null

cat >"$state/nodes.yaml" <<YAML
nodes:
  - id: production-node
    name: Production Node
    url: http://127.0.0.1:${node_port}/chat
    token: ${node_token}
YAML
env -u TERM_LLM_PPROF -u TERM_LLM_SERVE_HUB_URL -u TERM_LLM_SERVE_HUB_REGISTER \
  -u TERM_LLM_SERVE_HUB_NODE_ID -u TERM_LLM_SERVE_HUB_NODE_NAME \
  "$binary" serve hub --host 127.0.0.1 --port "$hub_port" --base-path /hub \
  --auth bearer --token "$hub_token" --config "$state/nodes.yaml" --contain=false \
  --nodes-file "$state/nodes.json" >"$state/hub.log" 2>&1 &
hub_pid=$!
for _ in $(seq 1 80); do
  if curl -fsS "http://127.0.0.1:${hub_port}/healthz" >/dev/null 2>&1; then break; fi
  kill -0 "$hub_pid" 2>/dev/null || { cat "$state/hub.log" >&2; exit 1; }
  sleep .25
done
curl -fsS "http://127.0.0.1:${hub_port}/healthz" >/dev/null

cd "$root/frontend"
if [[ -z "${PLAYWRIGHT_CHROMIUM_EXECUTABLE:-}" ]] && command -v chromium >/dev/null 2>&1; then
  export PLAYWRIGHT_CHROMIUM_EXECUTABLE="$(command -v chromium)"
fi
TERM_LLM_SMOKE_URL="${hub_root}node/production-node/" \
TERM_LLM_HUB_SMOKE_ROOT="$hub_root" TERM_LLM_HUB_SMOKE_TOKEN="$hub_token" \
TERM_LLM_WEBRTC_RELAY_URL="$relay_url" \
npm run test:e2e -- --workers=1 --reporter=line --output="$results" e2e/hub-production.spec.ts "$@"
succeeded=true
