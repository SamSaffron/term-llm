#!/usr/bin/env bash
# Reproducibly provide the pinned CPython runtime used by the browser demo.
#
# Resolution order:
#   1. an existing (usually actions/cache-restored) archive matching the repo pin
#   2. a same-origin public copy of that exact pinned archive, hash-verified
#      locally before use (the public bytes are never trusted without the pin)
#   3. the deterministic Alpine APK recipe in scripts/fetch-python-runtime.sh
#
# The runtime archive stays gitignored. No upstream kernel/BIOS bytes are
# downloaded or redistributed here.
# shellcheck disable=SC1091,SC2154
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
# shellcheck source=lib.sh
source "$here/lib.sh"

usage() {
  cat <<'EOF'
Usage: ensure-python-runtime.sh [--repo DIR] [--no-public] [--no-recipe]

Populates experiments/browser-bonsai/web/runtime with the archive and metadata
whose SHA-256 exactly matches experiments/browser-bonsai/web/python-runtime.mjs.
EOF
}

allow_public=1
allow_recipe=1
while (($#)); do
  case $1 in
    --repo) repo=$2; shift 2 ;;
    --no-public) allow_public=0; shift ;;
    --no-recipe) allow_recipe=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) demo_die "unknown argument: $1" ;;
  esac
done

demo_require_command curl
demo_require_command python3
demo_require_command sha256sum
demo_require_command gzip

web="$repo/experiments/browser-bonsai/web"
pin_file="$web/python-runtime.mjs"
runtime_dir="$web/runtime"
archive="$runtime_dir/python-root.tar.gz"
metadata="$runtime_dir/python-runtime.json"
recipe="$repo/experiments/browser-bonsai/scripts/fetch-python-runtime.sh"
base_url=${PYTHON_RUNTIME_BASE_URL:-https://demo.term-llm.com/runtime}

[[ -f $pin_file ]] || demo_die "Python runtime pin file is missing: $pin_file"
pin_sha=$(sed -n "s/.*sha256: '\([0-9a-f]\{64\}\)'.*/\1/p" "$pin_file" | head -n 1)
pin_bytes=$(sed -n 's/.*[^A-Za-z]bytes: \([0-9][0-9]*\).*/\1/p' "$pin_file" | head -n 1)
[[ -n $pin_sha && -n $pin_bytes ]] || demo_die "cannot read the pinned Python runtime hash from $pin_file"

validate_files() {
  local dir=$1
  local actual_sha actual_bytes json_sha json_bytes
  [[ -f $dir/python-root.tar.gz && -f $dir/python-runtime.json ]] || return 1
  actual_sha=$(demo_sha256 "$dir/python-root.tar.gz")
  actual_bytes=$(demo_bytes "$dir/python-root.tar.gz")
  read -r json_sha json_bytes < <(python3 - "$dir/python-runtime.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding='utf-8') as handle:
    data = json.load(handle)
print(data['sha256'], int(data['bytes']))
PY
)
  [[ $actual_sha == "$pin_sha" && $actual_bytes == "$pin_bytes" ]] || return 1
  [[ $json_sha == "$pin_sha" && $json_bytes == "$pin_bytes" ]] || return 1
  return 0
}

if validate_files "$runtime_dir"; then
  demo_log "Python runtime cache hit: $pin_sha"
  exit 0
fi

mkdir -p "$runtime_dir"
work=$(mktemp -d "$runtime_dir/.hydrate.XXXXXX")
trap 'rm -rf -- "$work"' EXIT

install_valid() {
  local src=$1
  validate_files "$src" || demo_die "refusing to install an unverified Python runtime"
  mv -f -- "$src/python-root.tar.gz" "$archive"
  mv -f -- "$src/python-runtime.json" "$metadata"
  validate_files "$runtime_dir" || demo_die "installed Python runtime failed re-verification"
  demo_log "installed verified Python runtime: $pin_sha"
}

if (( allow_public )); then
  demo_log "Python runtime cache miss; trying hash-pinned public archive"
  if curl -fsSL --retry 3 --connect-timeout 20 --max-time 300 \
      "$base_url/python-root.tar.gz" -o "$work/python-root.tar.gz" &&
     curl -fsSL --retry 3 --connect-timeout 20 --max-time 300 \
      "$base_url/python-runtime.json" -o "$work/python-runtime.json"; then
    if validate_files "$work"; then
      install_valid "$work"
      exit 0
    fi
    demo_log "public Python runtime did not match the repository pin; ignoring it"
  else
    demo_log "public Python runtime is unavailable; falling back to the APK recipe"
  fi
fi

if (( allow_recipe )); then
  [[ -f $recipe ]] || demo_die "deterministic Python runtime recipe is missing: $recipe"
  demo_log "rebuilding Python runtime from pinned Alpine packages"
  # Preserve the repository pin even though the recipe rewrites its generated
  # companion file. A recipe result that differs from the pin is an error.
  pin_backup="$work/python-runtime.mjs.pin"
  cp -- "$pin_file" "$pin_backup"
  if ! bash "$recipe"; then
    cp -- "$pin_backup" "$pin_file"
    demo_die "pinned Alpine package recipe failed"
  fi
  cp -- "$pin_backup" "$pin_file"
  validate_files "$runtime_dir" ||
    demo_die "APK recipe produced a runtime that does not match the repository pin; update the pin intentionally"
  exit 0
fi

demo_die "Python runtime is missing and all hydration sources are disabled"
