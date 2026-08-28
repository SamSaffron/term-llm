#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <release-tag> <checksums-file>" >&2
    exit 2
fi

: "${HOMEBREW_TAP_DEPLOY_KEY:?HOMEBREW_TAP_DEPLOY_KEY is required}"

tag=$1
checksums_file=$2
repository=${HOMEBREW_TAP_REPOSITORY:-SamSaffron/homebrew-tap}
script_dir=$(CDPATH='' cd -- "$(dirname "$0")" && pwd)

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT INT TERM HUP
key_file="$tmp_dir/deploy-key"
tap_dir="$tmp_dir/homebrew-tap"

printf '%s\n' "$HOMEBREW_TAP_DEPLOY_KEY" >"$key_file"
chmod 600 "$key_file"
export GIT_SSH_COMMAND="ssh -i $key_file -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

git clone --depth 1 "git@github.com:${repository}.git" "$tap_dir"
"$script_dir/generate_homebrew_formula.sh" "$tag" "$checksums_file" "$tap_dir/Formula/term-llm.rb"

if git -C "$tap_dir" diff --quiet -- Formula/term-llm.rb; then
    echo "Homebrew formula is already current for $tag"
    exit 0
fi

git -C "$tap_dir" config user.name "term-llm release bot"
git -C "$tap_dir" config user.email "actions@github.com"
git -C "$tap_dir" add Formula/term-llm.rb
git -C "$tap_dir" commit -m "Update term-llm to $tag"
git -C "$tap_dir" push origin HEAD:main
