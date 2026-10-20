#!/usr/bin/env bash
# Assemble one immutable static browser-demo release from experiments/browser-bonsai/dist.
# Adds deterministic gzip siblings, a source build manifest, and a file manifest
# used by the remote preflight before the nginx document root is switched.
# shellcheck disable=SC1091,SC2154
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
# shellcheck source=lib.sh
source "$here/lib.sh"

usage() {
  cat <<'EOF'
Usage: prepare-release.sh --dist DIR --out DIR --tarball FILE --id ID \
         --git-sha SHA --git-ref REF --built-at TIMESTAMP \
         [--source-date-epoch N] [--python-pin FILE] [--model-pin FILE]

The output directory must not exist or must be empty. The tarball is written
outside the output directory and contains the release with leading "./" paths.
EOF
}

dist=$repo/experiments/browser-bonsai/dist
out=
tarball=
id=
git_sha=
git_ref=
built_at=
source_date_epoch=${SOURCE_DATE_EPOCH:-0}
python_pin=$repo/experiments/browser-bonsai/web/python-runtime.mjs
model_pin=$repo/experiments/browser-bonsai/web/inference.mjs

while (($#)); do
  case $1 in
    --dist) dist=$2; shift 2 ;;
    --out) out=$2; shift 2 ;;
    --tarball) tarball=$2; shift 2 ;;
    --id) id=$2; shift 2 ;;
    --git-sha) git_sha=$2; shift 2 ;;
    --git-ref) git_ref=$2; shift 2 ;;
    --built-at) built_at=$2; shift 2 ;;
    --source-date-epoch) source_date_epoch=$2; shift 2 ;;
    --python-pin) python_pin=$2; shift 2 ;;
    --model-pin) model_pin=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) demo_die "unknown argument: $1" ;;
  esac
done

demo_require_command python3
demo_require_command sha256sum
demo_require_command gzip
demo_require_command tar

[[ -n $out && -n $tarball && -n $id && -n $git_sha && -n $git_ref && -n $built_at ]] ||
  demo_die "--out, --tarball, --id, --git-sha, --git-ref, and --built-at are required"
demo_assert_release_id "$id"
[[ $git_sha =~ ^[0-9a-f]{40}$ ]] || demo_die "--git-sha must be a 40-character lowercase hex SHA"
[[ $source_date_epoch =~ ^[0-9]+$ ]] || demo_die "--source-date-epoch must be an integer"
[[ -d $dist ]] || demo_die "browser demo dist directory does not exist: $dist"
[[ -f $python_pin ]] || demo_die "Python runtime pin file does not exist: $python_pin"
[[ -f $model_pin ]] || demo_die "model pin file does not exist: $model_pin"
[[ ! -e $out ]] || demo_die "output directory already exists: $out"

model_url=$(sed -n "s/^export const MODEL_URL = '\([^']*\)'.*/\1/p" "$model_pin" | head -n 1)
model_bytes=$(sed -n 's/^export const MODEL_BYTES = \([0-9][0-9]*\).*/\1/p' "$model_pin" | head -n 1)
[[ $model_url =~ ^https://demo\.term-llm\.com/models/[A-Za-z0-9._-]+$ ]] ||
  demo_die "cannot read the pinned demo model URL from $model_pin"
[[ $model_bytes =~ ^[0-9]+$ ]] || demo_die "cannot read the pinned demo model size from $model_pin"
model_path="models/${model_url##*/}"

# Reject an accidental copy of upstream Linux assets before packaging. The page
# must continue to fetch these directly from upstream at runtime.
for forbidden in "${demo_forbidden_assets[@]}"; do
  if find "$dist" -type f -name "$forbidden" -print -quit | grep -q .; then
    demo_die "dist contains upstream Linux asset $forbidden; it must not be redistributed"
  fi
done
for file in "${demo_critical_files[@]}"; do
  [[ -s $dist/$file ]] || demo_die "critical dist file is missing or empty: $file"
done
if ! grep -q 'raw.githubusercontent.com/copy/v86' "$dist/linux-assets.mjs" ||
   ! grep -q 'i.copy.sh/buildroot-bzimage' "$dist/linux-assets.mjs"; then
  demo_die "linux-assets.mjs no longer fetches kernel/BIOS from upstream"
fi

demo_verify_python_runtime "$dist" "$python_pin"

mkdir -p "$(dirname -- "$out")" "$(dirname -- "$tarball")" "$out"
cp -a "$dist/." "$out/"
# Remove any stale gzip siblings from earlier local builds; they are rebuilt
# below so the release is self-consistent.
find "$out" -type f -name '*.gz' ! -name '*.tar.gz' -delete
chmod -R u+rwX,go+rX "$out"

# nginx uses gzip_static. Compress deterministic text and wasm payloads only;
# images, fonts, and the already-compressed Python runtime are left alone.
wasm_sha=$(demo_sha256 "$out/term-llm.wasm")
v86_sha=$(demo_sha256 "$out/v86.wasm")
python_sha=$(demo_sha256 "$out/runtime/python-root.tar.gz")
python_bytes=$(demo_bytes "$out/runtime/python-root.tar.gz")

cat >"$out/release.json" <<EOF
{
  "schema": 1,
  "id": "$id",
  "gitSha": "$git_sha",
  "gitRef": "$git_ref",
  "builtAt": "$built_at",
  "sourceDateEpoch": $source_date_epoch,
  "pythonRuntime": {
    "sha256": "$python_sha",
    "bytes": $python_bytes
  },
  "model": {
    "url": "$model_url",
    "path": "$model_path",
    "bytes": $model_bytes
  },
  "critical": {
    "term-llm.wasm": "$wasm_sha",
    "v86.wasm": "$v86_sha",
    "runtime/python-root.tar.gz": "$python_sha"
  }
}
EOF
chmod 0644 "$out/release.json"

while IFS= read -r -d '' file; do
  gzip -9 -n -c -- "$file" >"$file.gz"
  chmod 0644 "$file.gz"
done < <(
  find "$out" -type f \
    \( -name '*.html' -o -name '*.js' -o -name '*.mjs' -o -name '*.css' \
       -o -name '*.json' -o -name '*.txt' -o -name '*.md' -o -name '*.xml' \
       -o -name '*.webmanifest' -o -name '*.wasm' \) -print0
)

manifest_tmp=$(mktemp "$out.manifest.XXXXXX")
trap 'rm -f -- "${manifest_tmp:-}"' EXIT
(
  cd "$out"
  find . -type f ! -name manifest.sha256 -print0 |
    LC_ALL=C sort -z |
    xargs -0 sha256sum
) >"$manifest_tmp"
mv -f -- "$manifest_tmp" "$out/manifest.sha256"
trap - EXIT
chmod 0644 "$out/manifest.sha256"

demo_verify_release "$out" "$id"

tmp_tar="$tarball.tmp.$$"
trap 'rm -f -- "$tmp_tar"' EXIT
tar --sort=name --mtime="@$source_date_epoch" --owner=0 --group=0 --numeric-owner --format=gnu \
  -cf - -C "$out" . | gzip -n -9 >"$tmp_tar"
mv -f -- "$tmp_tar" "$tarball"
trap - EXIT

demo_log "prepared release $id"
demo_log "  files: $(find "$out" -type f | wc -l | tr -d ' ')"
demo_log "  bytes: $(du -sk "$out" | awk '{print $1 * 1024}')"
demo_log "  tarball: $tarball ($(demo_bytes "$tarball") bytes)"
