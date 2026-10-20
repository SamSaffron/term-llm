#!/usr/bin/env bash
# Remote half of the browser demo deployment. Runs on the origin host only after
# the release tarball has been uploaded. It validates every byte, swaps the
# document-root symlink atomically, smoke-tests origin HTTPS, and rolls back on
# any failure. nginx, TLS, ACME, and the models directory are never modified.
# shellcheck disable=SC1091,SC2154
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$here/lib.sh"

usage() {
  cat <<'EOF'
Usage: remote-deploy.sh --root DIR --tarball FILE --id ID \
         [--keep N] [--cleanup-script FILE]

The tarball must contain a release with a ./release.json and ./manifest.sha256.
Set DEMO_HTTP_BASE/DEMO_HTTP_RESOLVE to override the origin HTTPS smoke target.
EOF
}

root=$demo_default_root
tarball=
id=
keep=3
cleanup_script=
while (($#)); do
  case $1 in
    --root) root=$2; shift 2 ;;
    --tarball) tarball=$2; shift 2 ;;
    --id) id=$2; shift 2 ;;
    --keep) keep=$2; shift 2 ;;
    --cleanup-script) cleanup_script=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) demo_die "unknown argument: $1" ;;
  esac
done

[[ -n $tarball && -n $id ]] || demo_die "--tarball and --id are required"
demo_assert_release_id "$id"
[[ $keep =~ ^[0-9]+$ ]] || demo_die "--keep must be a positive integer"
(( keep >= 2 )) || keep=2
demo_require_command flock
demo_require_command python3
demo_require_command sha256sum
demo_require_command tar
demo_require_command curl

demo_assert_root "$root"
root=$DEMO_ROOT_RESOLVED
releases="$root/releases"
[[ -d $releases && ! -L $releases ]] || demo_die "invalid releases directory: $releases"
[[ -f $tarball ]] || demo_die "release tarball does not exist: $tarball"
tarball=$(readlink -f -- "$tarball")
case $tarball in
  "$releases"/.incoming-*) ;;
  *) demo_die "refusing tarball outside $releases/.incoming-*: $tarball" ;;
esac

# Bound concurrent deploys on the host as well as in GitHub Actions.
exec 9>"$root/.deploy.lock"
flock -w "${DEMO_LOCK_TIMEOUT:-900}" 9 || demo_die "another deployment holds $root/.deploy.lock"

tar_kb=$(( ($(demo_bytes "$tarball") + 1023) / 1024 ))
avail_kb=$(df -Pk "$root" | awk 'NR == 2 { print $4 }')
need_kb=$((tar_kb * 4 + 51200))
(( avail_kb >= need_kb )) ||
  demo_die "insufficient free space at $root: ${avail_kb}KiB available, ${need_kb}KiB required"

staging="$releases/.staging-$id"
dest="$releases/$id"
previous_target=
swapped=0
smoke_dir=
if [[ -e $dest || -L $dest ]]; then
  demo_die "release already exists: $dest"
fi

on_exit() {
  local rc=$? resumed=0
  if (( rc != 0 )); then
    if (( swapped == 1 )); then
      demo_log "deployment failed; restoring previous document root"
      if [[ -n $previous_target ]]; then
        ln -sfn -- "$previous_target" "$root/.current.$$.rollback" &&
          mv -Tf -- "$root/.current.$$.rollback" "$root/current" || resumed=1
        if (( resumed == 0 )); then
          demo_log "rolled back to $previous_target"
        else
          demo_log "error: rollback could not restore $previous_target"
        fi
      else
        rm -f -- "$root/current" || resumed=1
      fi
    fi
    if [[ -n ${dest:-} && -d $dest && ! -L $dest ]]; then
      if [[ $(readlink -f -- "$root/current" 2>/dev/null || true) != "$(readlink -f -- "$dest")" ]]; then
        rm -rf --one-file-system -- "$dest" || true
      fi
    fi
  fi
  if [[ -n ${staging:-} && -d $staging && ! -L $staging ]]; then
    rm -rf --one-file-system -- "$staging" || true
  fi
  if [[ -n ${smoke_dir:-} && -d $smoke_dir && ! -L $smoke_dir ]]; then
    rm -rf --one-file-system -- "$smoke_dir" || true
  fi
  rm -f -- "$tarball" "$root/.current.$$.new" "$root/.current.$$.rollback" || true
  if [[ -n ${DEMO_TEMP_DIR:-} && -d $DEMO_TEMP_DIR ]]; then
    rm -rf --one-file-system -- "$DEMO_TEMP_DIR" || true
  fi
  exit "$rc"
}
trap on_exit EXIT

rm -rf --one-file-system -- "$staging"
mkdir -m 0755 -- "$staging"

# Reject absolute, parent-traversing, and non-"./" tar entries before extraction.
tar -tzf "$tarball" >"$staging/.tar-list"
if grep -Eq '(^/|(^|/)\.\.(/|$))' "$staging/.tar-list"; then
  demo_die "release tarball contains an unsafe path"
fi
if grep -Evq '^\./?$|^\./' "$staging/.tar-list"; then
  demo_die "release tarball contains an entry outside ./"
fi
rm -f -- "$staging/.tar-list"

# Reject symlinks, hardlinks, devices, and FIFOs before extracting as root.
LC_ALL=C tar -tvzf "$tarball" >"$staging/.tar-types"
if grep -Evq '^[-d]' "$staging/.tar-types"; then
  demo_die "release tarball contains a non-regular entry"
fi
rm -f -- "$staging/.tar-types"
tar -xzf "$tarball" --no-same-owner --no-same-permissions -C "$staging"
chmod -R u+rwX,go+rX "$staging"

demo_verify_release "$staging" "$id"

read -r model_path model_bytes < <(python3 - "$staging/release.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding='utf-8') as handle:
    data = json.load(handle)
model = data['model']
print(model['path'], int(model['bytes']))
PY
)
[[ $model_path =~ ^models/[A-Za-z0-9._-]+$ ]] || demo_die "invalid pinned model path: $model_path"
[[ $model_bytes =~ ^[0-9]+$ ]] || demo_die "invalid pinned model size: $model_bytes"
[[ -f $root/$model_path && ! -L $root/$model_path ]] ||
  demo_die "pinned model is missing from the origin: $root/$model_path"
[[ $(demo_bytes "$root/$model_path") == "$model_bytes" ]] ||
  demo_die "pinned model size mismatch: $(demo_bytes "$root/$model_path") != $model_bytes"

demo_smoke() {
  local url=${DEMO_HTTP_BASE:-https://demo.term-llm.com}
  local resolve=${DEMO_HTTP_RESOLVE:-demo.term-llm.com:443:127.0.0.1}
  local curl_bin=${DEMO_CURL:-curl}
  local -a args=(
    --fail --silent --show-error
    --connect-timeout 10 --max-time 60
    --resolve "$resolve"
    --noproxy '*'
    --header 'Cache-Control: no-cache'
    --header 'Pragma: no-cache'
  )
  local page release_path actual expected code size
  local -a paths=(term-llm.wasm v86.wasm runtime/python-root.tar.gz)

  page=$("$curl_bin" "${args[@]}" "$url/?deploy=$id")
  grep -q 'boot.mjs' <<<"$page" || demo_die "origin smoke: index does not load boot.mjs"
  release_path=$("$curl_bin" "${args[@]}" "$url/release.json?deploy=$id")
  grep -Fq "\"id\": \"$id\"" <<<"$release_path" || demo_die "origin smoke: serving a different release"

  for path in "${paths[@]}"; do
    expected=$(demo_sha256 "$dest/$path")
    actual=$("$curl_bin" "${args[@]}" "$url/$path?deploy=$id" | sha256sum | awk '{print $1}')
    [[ $actual == "$expected" ]] || demo_die "origin smoke: $path hash mismatch ($actual != $expected)"
    demo_log "origin smoke ok: $path"
  done

  # Range requests are required by the model loader. A valid 206 may omit
  # Accept-Ranges (Cloudflare does); Content-Range is the authoritative check.
  # HEAD Accept-Ranges remains advisory only.
  range_check() {
    local path=$1 expected_total=$2 headers="$smoke_dir/headers" body="$smoke_dir/body" header_text
    code=$("$curl_bin" "${args[@]}" --range 0-1023 \
      --dump-header "$headers" --output "$body" --write-out '%{http_code}' \
      "$url/$path?deploy=$id")
    [[ $code == 206 ]] || demo_die "origin smoke: $path returned HTTP $code for a range request; expected 206"
    header_text=$(tr -d '\r' <"$headers")
    grep -Eiq "^content-range:[[:space:]]*bytes 0-1023/$expected_total$" <<<"$header_text" ||
      demo_die "origin smoke: $path 206 response has no valid Content-Range for $expected_total bytes"
    size=$(demo_bytes "$body")
    [[ $size == 1024 ]] || demo_die "origin smoke: $path range body is $size bytes; expected 1024"
    demo_log "origin smoke ok: $path range 206 Content-Range"
  }

  head_accept_ranges() {
    local path=$1 headers="$smoke_dir/head-headers" header_text
    "$curl_bin" "${args[@]}" --head --output "$headers" "$url/$path?deploy=$id" >/dev/null
    header_text=$(tr -d '\r' <"$headers")
    if grep -Eiq '^accept-ranges:[[:space:]]*bytes' <<<"$header_text"; then
      demo_log "origin smoke advisory: $path HEAD advertises Accept-Ranges: bytes"
    else
      demo_log "origin smoke advisory: $path HEAD omits Accept-Ranges (allowed)"
    fi
  }

  range_check term-llm.wasm "$(demo_bytes "$dest/term-llm.wasm")"
  range_check "$model_path" "$model_bytes"
  head_accept_ranges "$model_path"
}

current_target=$(readlink -- "$root/current" 2>/dev/null || true)
case $current_target in
  releases/*)
    previous_name=${current_target#releases/}
    [[ -n $previous_name && $previous_name != */* ]] ||
      demo_die "current symlink is not a direct release child: $current_target"
    previous_target=$current_target
    ;;
  '') previous_target=
      previous_name=
      ;;
  *) demo_die "current symlink points outside releases/: $current_target" ;;
esac

mv -- "$staging" "$dest"
ln -sfn -- "releases/$id" "$root/.current.$$.new"
mv -Tf -- "$root/.current.$$.new" "$root/current"
swapped=1
demo_log "document root now releases/$id"

smoke_dir=$(mktemp -d "$root/.smoke.XXXXXX")
demo_smoke
# The release is committed once origin HTTPS serves exactly these bytes.
swapped=0
rm -rf --one-file-system -- "$smoke_dir"
smoke_dir=
if [[ -n $previous_target ]]; then
  demo_log "previous release retained: $previous_target"
fi

if [[ -n $cleanup_script ]]; then
  [[ -f $cleanup_script ]] || demo_die "cleanup script does not exist: $cleanup_script"
  cleanup_args=(--root "$root" --keep "$keep")
  if [[ -n $previous_name ]]; then
    cleanup_args+=(--protect "$previous_name")
  fi
  if ! bash "$cleanup_script" "${cleanup_args[@]}"; then
    demo_log "warning: bounded release cleanup failed; the new release is live"
  fi
fi

demo_log "deployment complete: $id"
