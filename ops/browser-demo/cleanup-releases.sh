#!/usr/bin/env bash
# Keep the demo host bounded without touching anything outside the release
# directory. Only workflow-owned release names are eligible for deletion, and
# the live current release is always protected.
# shellcheck disable=SC1091,SC2154
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$here/lib.sh"

usage() {
  cat <<'EOF'
Usage: cleanup-releases.sh [--root DIR] [--keep N] [--protect NAME]... [--dry-run]

Removes old workflow-created releases under DIR/releases, keeping at least the
current release, the newest N timestamp/SHA releases (default 3), and any
explicitly protected direct child (for example the immediate rollback target).
Directories with other names, symlinks, and anything outside DIR/releases are
never touched.
EOF
}

root=$demo_default_root
keep=3
dry_run=0
protect_names=()
while (($#)); do
  case $1 in
    --root) root=$2; shift 2 ;;
    --keep) keep=$2; shift 2 ;;
    --protect) protect_names+=("$2"); shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) demo_die "unknown argument: $1" ;;
  esac
done

[[ $keep =~ ^[0-9]+$ ]] || demo_die "--keep must be a positive integer"
(( keep >= 2 )) || keep=2

demo_assert_root "$root"
root=$DEMO_ROOT_RESOLVED
releases="$root/releases"
[[ -d $releases ]] || demo_die "releases directory does not exist: $releases"
[[ ! -L $releases ]] || demo_die "refusing to use a symlinked releases directory: $releases"

current=
if [[ -L $root/current ]]; then
  target=$(readlink -- "$root/current")
  case $target in
    releases/*) current=${target#releases/} ;;
    *) demo_die "current symlink points outside releases/: $target" ;;
  esac
  [[ -n $current && $current != . && $current != .. && $current != */* ]] ||
    demo_die "current symlink has an unexpected release name: $current"
  [[ -d $releases/$current && ! -L $releases/$current ]] ||
    demo_die "current release directory is missing or invalid: $releases/$current"
fi

mapfile -t candidates < <(
  find "$releases" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' |
    grep -E "$demo_release_re" | LC_ALL=C sort -r
)

declare -A protected=()
for ((i = 0; i < keep && i < ${#candidates[@]}; i++)); do
  protected["${candidates[i]}"]=1
done
if [[ -n $current ]]; then
  protected[$current]=1
fi

# The immediate rollback target may be older than the newest N releases after a
# manual rollback. Accept only a direct child directory, never a path.
for name in "${protect_names[@]}"; do
  [[ -n $name && $name != . && $name != .. && $name != */* ]] ||
    demo_die "invalid --protect name: $name"
  target="$releases/$name"
  [[ -d $target && ! -L $target ]] ||
    demo_die "protected release is missing or not a directory: $target"
  resolved=$(realpath -m -- "$target")
  [[ $resolved == "$releases/$name" ]] ||
    demo_die "refusing protected path outside releases/: $target"
  protected[$name]=1
done

removed=0
for name in "${candidates[@]}"; do
  if [[ -n ${protected[$name]:-} ]]; then
    continue
  fi
  target="$releases/$name"
  [[ -d $target && ! -L $target ]] || continue
  resolved=$(realpath -m -- "$target")
  [[ $resolved == "$releases/$name" ]] || demo_die "refusing path that escapes releases/: $target"
  [[ $(dirname -- "$resolved") == "$releases" ]] || demo_die "refusing non-direct child: $target"

  if (( dry_run )); then
    demo_log "would remove release $resolved"
  else
    demo_log "removing release $resolved"
    rm -rf --one-file-system -- "$resolved"
  fi
  removed=$((removed + 1))
done

demo_log "release cleanup complete: $removed removed, ${#protected[@]} protected, ${#candidates[@]} workflow releases present"
