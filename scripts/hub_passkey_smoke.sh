#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ ! -x "$root/frontend/node_modules/.bin/playwright" ]]; then
  echo "frontend smoke dependencies are missing; run 'make frontend-deps' first" >&2
  exit 1
fi
free_port(){ node -e 'const net=require("node:net");const s=net.createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close();});'; }
public_port="${TERM_LLM_HUB_SMOKE_PORT:-$(free_port)}"
backend_port="$(free_port)"
url="https://localhost:${public_port}/hub/"
token="virtual-passkey-bootstrap-secret"
bearer_token="optional-passkey-api-bearer-secret"
binary="$(mktemp "${TMPDIR:-/tmp}/term-llm-hub-passkey.XXXXXX")"
proxy_binary="$(mktemp "${TMPDIR:-/tmp}/term-llm-hub-proxy.XXXXXX")"
log="$(mktemp "${TMPDIR:-/tmp}/term-llm-hub-passkey.XXXXXX.log")"
state="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-hub-passkey-state.XXXXXX")"
results="$(mktemp -d "${TMPDIR:-/tmp}/term-llm-hub-passkey-results.XXXXXX")"
succeeded=false
cleanup(){
  if [[ -n "${server_pid:-}" ]];then kill "$server_pid" 2>/dev/null||true;fi
  if [[ -n "${proxy_pid:-}" ]];then kill "$proxy_pid" 2>/dev/null||true;fi
  if [[ "$succeeded" != true && -s "$log" ]];then cat "$log" >&2;fi
  rm -f "$binary" "$proxy_binary" "$log";rm -rf "$state" "$results"
}
trap cleanup EXIT
wait_for_hub(){
  for _ in $(seq 1 80);do
    if curl -fsS "http://127.0.0.1:${backend_port}/healthz" >/dev/null 2>&1;then return;fi
    if ! kill -0 "$server_pid" 2>/dev/null;then cat "$log" >&2;exit 1;fi
    sleep .25
  done
  curl -fsS "http://127.0.0.1:${backend_port}/healthz" >/dev/null
}

cd "$root"
npm --prefix frontend run build
go build -o "$binary" .
cat >"$state/proxy.go" <<'GO'
package main
import("log";"net/http";"net/http/httputil";"net/url";"os")
func main(){target,err:=url.Parse(os.Args[1]);if err!=nil{log.Fatal(err)};log.Fatal(http.ListenAndServeTLS(os.Args[2],os.Args[3],os.Args[4],httputil.NewSingleHostReverseProxy(target)))}
GO
go build -o "$proxy_binary" "$state/proxy.go"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=localhost' -addext 'subjectAltName=DNS:localhost' -keyout "$state/key.pem" -out "$state/cert.pem" >/dev/null 2>&1
"$proxy_binary" "http://127.0.0.1:${backend_port}" "127.0.0.1:${public_port}" "$state/cert.pem" "$state/key.pem" >>"$log" 2>&1 &
proxy_pid=$!

env -u TERM_LLM_PPROF TERM_LLM_HUB_TOKEN="$bearer_token" TERM_LLM_HUB_BOOTSTRAP_TOKEN="$token" "$binary" serve hub \
  --auth passkey --public-url "$url" --base-path /hub --passkey-trusted-proxy 127.0.0.1/32 \
  --host 127.0.0.1 --port "$backend_port" --contain=false \
  --nodes-file "$state/nodes.json" --passkey-auth-file "$state/auth/auth.json" >"$log" 2>&1 &
server_pid=$!
wait_for_hub
curl -kfsS "https://localhost:${public_port}/healthz" >/dev/null
if grep -Fq "$token" "$log" || grep -Fq "$bearer_token" "$log";then echo "bootstrap or bearer secret leaked to Hub output" >&2;exit 1;fi
if grep -Fq "generated Hub bearer token" "$log";then echo "passkey mode generated a bearer token" >&2;exit 1;fi
if ! grep -Fq "explicit bearer API compatibility: enabled (from TERM_LLM_HUB_TOKEN)" "$log";then echo "passkey bearer compatibility was not disclosed" >&2;exit 1;fi

cd "$root/frontend"
if [[ -z "${PLAYWRIGHT_CHROMIUM_EXECUTABLE:-}" ]] && command -v chromium >/dev/null 2>&1; then
  export PLAYWRIGHT_CHROMIUM_EXECUTABLE="$(command -v chromium)"
fi
TERM_LLM_HUB_SMOKE_URL="$url" TERM_LLM_HUB_SMOKE_TOKEN="$token" TERM_LLM_HUB_CREDENTIAL_FILE="$state/credential.json" \
  npm run test:e2e -- --project=desktop --workers=1 --reporter=line --output="$results" e2e/hub-passkey.spec.ts

kill "$server_pid";wait "$server_pid" 2>/dev/null||true;server_pid=""
recovery_token="virtual-passkey-recovery-secret"
env -u TERM_LLM_PPROF TERM_LLM_HUB_TOKEN="$bearer_token" TERM_LLM_HUB_RECOVERY_TOKEN="$recovery_token" "$binary" serve hub \
  --auth passkey --public-url "$url" --base-path /hub --passkey-trusted-proxy 127.0.0.1/32 \
  --host 127.0.0.1 --port "$backend_port" --contain=false \
  --nodes-file "$state/nodes.json" --passkey-auth-file "$state/auth/auth.json" >>"$log" 2>&1 &
server_pid=$!
wait_for_hub
TERM_LLM_HUB_SMOKE_URL="$url" TERM_LLM_HUB_SMOKE_TOKEN="$recovery_token" TERM_LLM_HUB_CREDENTIAL_FILE="$state/credential.json" \
  npm run test:e2e -- --project=desktop --workers=1 --reporter=line --output="$results" e2e/hub-recovery.spec.ts
if grep -Fq "$token" "$log"||grep -Fq "$recovery_token" "$log"||grep -Fq "$bearer_token" "$log";then echo "passkey host secret leaked to Hub output" >&2;exit 1;fi
succeeded=true
