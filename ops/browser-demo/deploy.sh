#!/usr/bin/env bash
# Client half of the browser demo deployment. Uploads a prepared release over
# SSH with a pinned, verified host key, then runs remote-deploy.sh. The private
# key is read from DEPLOY_SSH_KEY_FILE or DEPLOY_SSH_KEY and is never echoed or
# placed in an argument list.
# shellcheck disable=SC1091,SC2154
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
source "$here/lib.sh"

usage() {
  cat <<'EOF'
Usage: deploy.sh --tarball FILE --id ID [--host HOST] [--user USER]
         [--root DIR] [--keep N] [--known-hosts FILE]

Environment:
  DEPLOY_SSH_KEY_FILE  path to the private key (preferred)
  DEPLOY_SSH_KEY       private key contents; written to a temporary 0600 file
  DEMO_SSH, DEMO_SCP   command overrides for local tests
EOF
}

host=143.198.76.107
user=root
root=$demo_default_root
tarball=
id=
keep=3
known_hosts=$here/known_hosts
while (($#)); do
  case $1 in
    --host) host=$2; shift 2 ;;
    --user) user=$2; shift 2 ;;
    --root) root=$2; shift 2 ;;
    --tarball) tarball=$2; shift 2 ;;
    --id) id=$2; shift 2 ;;
    --keep) keep=$2; shift 2 ;;
    --known-hosts) known_hosts=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) demo_die "unknown argument: $1" ;;
  esac
done

[[ -n $tarball && -n $id ]] || demo_die "--tarball and --id are required"
demo_assert_release_id "$id"
[[ $keep =~ ^[0-9]+$ ]] || demo_die "--keep must be a positive integer"
(( keep >= 2 )) || keep=2
[[ -f $tarball ]] || demo_die "release tarball does not exist: $tarball"
tarball=$(readlink -f -- "$tarball")
[[ -f $known_hosts ]] || demo_die "known_hosts file does not exist: $known_hosts"
known_hosts=$(readlink -f -- "$known_hosts")

# The client cannot resolve a remote path, but the same hard exclusions apply.
case "$root/" in
  /var/www/term-llm/*) demo_die "refusing to deploy over the docs webroot: $root" ;;
esac
if [[ $root != "$demo_default_root" && ${DEMO_ALLOW_CUSTOM_ROOT:-0} != 1 ]]; then
  demo_die "refusing unexpected deployment root $root (set DEMO_ALLOW_CUSTOM_ROOT=1 only for tests)"
fi

demo_require_command ssh
demo_require_command scp
demo_require_command ssh-keygen

# Exactly one pinned host key, with the fingerprint recorded in lib.sh.
host_key_matches=$(ssh-keygen -F "$host" -f "$known_hosts" -l 2>/dev/null | grep -vc '^#' || true)
[[ $host_key_matches == 1 ]] || demo_die "expected exactly one pinned host key for $host in $known_hosts"
host_fingerprint=$(ssh-keygen -F "$host" -f "$known_hosts" -l 2>/dev/null | grep -o 'SHA256:[^ ]*' | head -n 1 || true)
[[ $host_fingerprint == "$demo_host_key_fingerprint" ]] ||
  demo_die "host key fingerprint mismatch for $host: $host_fingerprint"

created_key=0
key_file=${DEPLOY_SSH_KEY_FILE:-}
if [[ -z $key_file ]]; then
  [[ -n ${DEPLOY_SSH_KEY:-} ]] || demo_die "set DEPLOY_SSH_KEY_FILE or DEPLOY_SSH_KEY"
  key_file=$(mktemp "${TMPDIR:-/tmp}/browser-demo-deploy-key.XXXXXX")
  created_key=1
  printf '%s\n' "$DEPLOY_SSH_KEY" >"$key_file"
fi
[[ -f $key_file ]] || demo_die "SSH key file does not exist: $key_file"
key_mode=$(stat -c '%a' -- "$key_file")
[[ $key_mode =~ ^[0-7]00$ ]] || demo_die "SSH key file must be owner-only (0600): $key_file"
if (( created_key )); then
  trap 'rm -f -- "$key_file"' EXIT
fi

ssh_bin=${DEMO_SSH:-ssh}
scp_bin=${DEMO_SCP:-scp}
ssh_opts=(
  -i "$key_file"
  -o IdentitiesOnly=yes
  -o BatchMode=yes
  -o PasswordAuthentication=no
  -o KbdInteractiveAuthentication=no
  -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile="$known_hosts"
  -o GlobalKnownHostsFile=/dev/null
  -o ConnectTimeout=20
  -o ServerAliveInterval=15
  -o ServerAliveCountMax=4
)
scp_opts=(
  -q
  -i "$key_file"
  -o IdentitiesOnly=yes
  -o BatchMode=yes
  -o PasswordAuthentication=no
  -o KbdInteractiveAuthentication=no
  -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile="$known_hosts"
  -o GlobalKnownHostsFile=/dev/null
  -o ConnectTimeout=20
)

remote_tmp="$root/releases/.deploy-$id"
incoming="$root/releases/.incoming-$id.tar.gz"

ssh_run() {
  "$ssh_bin" "${ssh_opts[@]}" "$user@$host" "$@"
}

ssh_run "umask 077; mkdir -p -- '$root/releases' '$remote_tmp'"
if ! "$scp_bin" "${scp_opts[@]}" -- "$tarball" "$user@$host:$incoming"; then
  ssh_run "rm -rf -- '$remote_tmp' '$incoming'" >/dev/null 2>&1 || true
  demo_die "release upload failed"
fi
if ! "$scp_bin" "${scp_opts[@]}" -- \
    "$here/lib.sh" "$here/remote-deploy.sh" "$here/cleanup-releases.sh" \
    "$user@$host:$remote_tmp/"; then
  ssh_run "rm -rf -- '$remote_tmp' '$incoming'" >/dev/null 2>&1 || true
  demo_die "deployment script upload failed"
fi

set +e
ssh_run "DEMO_TEMP_DIR='$remote_tmp' bash '$remote_tmp/remote-deploy.sh' --root '$root' --tarball '$incoming' --id '$id' --keep '$keep' --cleanup-script '$remote_tmp/cleanup-releases.sh'"
rc=$?
set -e
if (( rc != 0 )); then
  ssh_run "rm -rf -- '$remote_tmp' '$incoming'" >/dev/null 2>&1 || true
  demo_log "deployment failed remotely; the previous release remains current"
  exit "$rc"
fi

ssh_run "rm -rf -- '$remote_tmp'" >/dev/null 2>&1 || demo_log "warning: could not remove $remote_tmp"
demo_log "deployed release $id to $root"
