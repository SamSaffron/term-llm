#!/usr/bin/env bash
# Shared safety, validation, and manifest helpers for the browser demo deploy.
# This file is sourced by the build and remote deployment scripts.
# shellcheck disable=SC2034,SC2154
set -euo pipefail

demo_default_root=/var/www/term-llm-demo
# Release IDs are UTC timestamps plus a short source SHA, e.g.
# 20261020T172300Z-0123456789ab. Cleanup only ever considers this shape.
demo_release_re='^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}$'
# Verified from the operator's SSH known_hosts for 143.198.76.107.
demo_host_key_fingerprint='SHA256:hFtk253jgXtR0xarGb8QujeDOippF1uE53pfMNn0/5c'

# Files that must exist and pass format/hash checks before a release may be
# swapped into service. Keep this list in sync with the browser demo output.
demo_critical_files=(
  index.html
  term-llm.wasm
  wasm_exec.js
  boot.mjs
  worker.mjs
  transport.mjs
  inference.bundle.mjs
  linux.mjs
  linux-client.mjs
  linux-worker.mjs
  linux-assets.mjs
  python-runtime.mjs
  libv86.mjs
  v86.wasm
  runtime/python-root.tar.gz
  runtime/python-runtime.json
  assets/asset-manifest.json
  manifest.webmanifest
  icon-512.png
)

# The Linux kernel and firmware are fetched by the browser directly from
# upstream. They must never be copied into a release.
demo_forbidden_assets=(
  seabios.bin
  vgabios.bin
  linux.bin
  bzimage
)

demo_log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

demo_die() {
  demo_log "error: $*"
  exit 1
}

demo_require_command() {
  local name=$1
  command -v "$name" >/dev/null 2>&1 || demo_die "required command not found: $name"
}

demo_sha256() {
  sha256sum -- "$1" | awk '{print $1}'
}

demo_bytes() {
  stat -c '%s' -- "$1"
}

demo_magic() {
  od -An -tx1 -N4 -- "$1" | tr -d ' \n'
}

demo_assert_release_id() {
  [[ $1 =~ $demo_release_re ]] || demo_die "invalid release id: $1"
}

# Resolve a deployment root and refuse anything that could overlap the docs
# webroot. Custom roots are allowed only for local tests and must be explicit.
demo_assert_root() {
  local root=$1 resolved
  [[ -n $root ]] || demo_die "deployment root is empty"
  [[ -d $root ]] || demo_die "deployment root does not exist: $root"
  resolved=$(readlink -f -- "$root") || demo_die "cannot resolve deployment root: $root"
  case "$resolved/" in
    /var/www/term-llm/*) demo_die "refusing to touch the docs webroot: $resolved" ;;
  esac
  if [[ $resolved != "$demo_default_root" && ${DEMO_ALLOW_CUSTOM_ROOT:-0} != 1 ]]; then
    demo_die "refusing unexpected deployment root $resolved (set DEMO_ALLOW_CUSTOM_ROOT=1 only for tests)"
  fi
  DEMO_ROOT_RESOLVED=$resolved
}

# Validate the CPython runtime archive against the repository pin and the
# generated runtime metadata. The pin is authoritative; JSON only corroborates.
demo_verify_python_runtime() {
  local dir=$1 pin_file=${2:-$1/python-runtime.mjs}
  local pin_sha pin_bytes json_sha json_bytes actual_sha actual_bytes
  [[ -f $pin_file ]] || demo_die "Python runtime pin file is missing: $pin_file"
  [[ -f $dir/runtime/python-root.tar.gz ]] || demo_die "Python runtime archive is missing"
  [[ -f $dir/runtime/python-runtime.json ]] || demo_die "Python runtime metadata is missing"

  pin_sha=$(sed -n "s/.*sha256: '\([0-9a-f]\{64\}\)'.*/\1/p" "$pin_file" | head -n 1)
  pin_bytes=$(sed -n 's/.*[^A-Za-z]bytes: \([0-9][0-9]*\).*/\1/p' "$pin_file" | head -n 1)
  [[ -n $pin_sha && -n $pin_bytes ]] || demo_die "cannot read the pinned Python runtime hash from $pin_file"

  read -r json_sha json_bytes < <(python3 - "$dir/runtime/python-runtime.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding='utf-8') as handle:
    data = json.load(handle)
print(data['sha256'], int(data['bytes']))
PY
)
  actual_sha=$(demo_sha256 "$dir/runtime/python-root.tar.gz")
  actual_bytes=$(demo_bytes "$dir/runtime/python-root.tar.gz")

  [[ $actual_sha == "$pin_sha" ]] || demo_die "Python runtime SHA-256 mismatch: $actual_sha != pinned $pin_sha"
  [[ $actual_bytes == "$pin_bytes" ]] || demo_die "Python runtime size mismatch: $actual_bytes != pinned $pin_bytes"
  [[ $json_sha == "$pin_sha" ]] || demo_die "Python runtime metadata SHA-256 mismatch: $json_sha != pinned $pin_sha"
  [[ $json_bytes == "$pin_bytes" ]] || demo_die "Python runtime metadata size mismatch: $json_bytes != pinned $pin_bytes"
}

# Validate a fully assembled release directory. This is used by both prepare
# (before packaging) and remote-deploy (before the symlink swap).
demo_verify_release() {
  local dir=$1 expected_id=${2:-}
  local file release_id git_sha built_at py_sha py_bytes model_path model_bytes model_url
  [[ -d $dir ]] || demo_die "release directory does not exist: $dir"
  [[ -f $dir/release.json ]] || demo_die "release manifest is missing"
  [[ -f $dir/manifest.sha256 ]] || demo_die "file manifest is missing"

  read -r release_id git_sha built_at py_sha py_bytes model_path model_bytes model_url < <(python3 - "$dir/release.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding='utf-8') as handle:
    data = json.load(handle)
runtime = data.get('pythonRuntime', {})
model = data.get('model', {})
print(data.get('id', ''), data.get('gitSha', ''), data.get('builtAt', ''),
      runtime.get('sha256', ''), runtime.get('bytes', ''),
      model.get('path', ''), model.get('bytes', ''), model.get('url', ''))
PY
)
  [[ -n $release_id ]] || demo_die "release manifest has no id"
  [[ $release_id =~ $demo_release_re ]] || demo_die "release manifest has an invalid id"
  [[ $git_sha =~ ^[0-9a-f]{40}$ ]] || demo_die "release manifest has an invalid git SHA"
  [[ -n $built_at ]] || demo_die "release manifest has no build time"
  [[ $model_path =~ ^models/[A-Za-z0-9._-]+$ ]] || demo_die "release manifest has an invalid model path"
  [[ $model_bytes =~ ^[0-9]+$ ]] || demo_die "release manifest has an invalid model size"
  [[ $model_url == "https://demo.term-llm.com/$model_path" ]] || demo_die "release manifest model URL is not the pinned same-origin path"
  if [[ -n $expected_id && $release_id != "$expected_id" ]]; then
    demo_die "release manifest id $release_id does not match requested id $expected_id"
  fi

  for file in "${demo_critical_files[@]}"; do
    [[ -s $dir/$file ]] || demo_die "critical release file is missing or empty: $file"
  done

  for file in "${demo_forbidden_assets[@]}"; do
    if find "$dir" -type f -name "$file" -print -quit | grep -q .; then
      demo_die "refusing to publish upstream Linux asset: $file"
    fi
  done
  if find "$dir" -type l -print -quit | grep -q .; then
    demo_die "release contains symlinks"
  fi
  if find "$dir" -type f -size +100M -print -quit | grep -q .; then
    demo_die "release contains an unexpectedly large file"
  fi
  if ! grep -q 'raw.githubusercontent.com/copy/v86' "$dir/linux-assets.mjs" ||
     ! grep -q 'i.copy.sh/buildroot-bzimage' "$dir/linux-assets.mjs"; then
    demo_die "linux-assets.mjs no longer fetches kernel/BIOS from upstream"
  fi

  [[ $(demo_magic "$dir/term-llm.wasm") == 0061736d ]] || demo_die "term-llm.wasm is not a WebAssembly module"
  [[ $(demo_magic "$dir/v86.wasm") == 0061736d ]] || demo_die "v86.wasm is not a WebAssembly module"
  [[ $(od -An -tx1 -N2 -- "$dir/runtime/python-root.tar.gz" | tr -d ' \n') == 1f8b ]] ||
    demo_die "Python runtime is not gzip data"

  demo_verify_python_runtime "$dir"
  [[ $py_sha == "$(demo_sha256 "$dir/runtime/python-root.tar.gz")" ]] ||
    demo_die "release manifest Python runtime SHA-256 is stale"
  [[ $py_bytes == "$(demo_bytes "$dir/runtime/python-root.tar.gz")" ]] ||
    demo_die "release manifest Python runtime size is stale"

  ( cd "$dir" && sha256sum -c manifest.sha256 --quiet ) ||
    demo_die "release file manifest verification failed"
}
