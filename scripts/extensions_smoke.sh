#!/usr/bin/env bash
# Clean-room extension UX exercise. Never touches the user's configuration.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
make build
home="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-extensions.XXXXXX")"
results="${TERM_LLM_EXTENSION_RESULTS:-$root/.cache/extensions-smoke}"
mkdir -p "$results" "$home/config/term-llm/extensions" "$home/workspace"
cleanup() {
  if [[ -n "${pid:-}" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  rm -rf "$home"
}
trap cleanup EXIT
cp -R examples/extensions/dracula examples/extensions/studio-clock "$home/config/term-llm/extensions/"
cat > "$home/config/term-llm/config.yaml" <<'YAML'
# Isolated extension UX fixture. No user credentials.
default_provider: debug
providers:
  debug:
    model: fast
YAML
printf 'enabled: []\n' > "$home/config/term-llm/extensions/extensions.yaml"
port="$(python3 - <<'PY'
import socket
with socket.socket() as s:
    s.bind(('127.0.0.1', 0))
    print(s.getsockname()[1])
PY
)"
(
 cd "$home/workspace"
 exec env -u TERM_LLM_PPROF -u TERM_LLM_SERVE_HUB_URL -u TERM_LLM_SERVE_HUB_REGISTER \
 HOME="$home" XDG_CONFIG_HOME="$home/config" XDG_DATA_HOME="$home/data" XDG_CACHE_HOME="$home/cache" TERM_LLM_SKIP_UPDATE_CHECK=1 \
 "$root/term-llm" serve web --no-auth --port "$port" --base-path /studio
) > "$results/server.log" 2>&1 &
pid=$!
for _ in $(seq 1 80); do
 if curl -fsS "http://127.0.0.1:$port/studio/healthz" >/dev/null 2>&1; then break; fi
 if ! kill -0 "$pid" 2>/dev/null; then cat "$results/server.log"; exit 1; fi
 sleep 0.2
done
export TERM_LLM_SMOKE_URL="http://127.0.0.1:$port/studio/"
export TERM_LLM_EXTENSION_DIR="$home/config/term-llm/extensions"
export TERM_LLM_EXTENSION_CONFIG="$home/config/term-llm/extensions/extensions.yaml"
export TERM_LLM_EXTENSION_MAIN_CONFIG="$home/config/term-llm/config.yaml"
if [[ -z "${PLAYWRIGHT_CHROMIUM_EXECUTABLE:-}" ]] && command -v chromium >/dev/null; then
 export PLAYWRIGHT_CHROMIUM_EXECUTABLE="$(command -v chromium)"
fi
npm --prefix frontend run test:e2e -- extensions.spec.ts --output="$results/browser" "$@"
echo "Clean-room results: $results"
