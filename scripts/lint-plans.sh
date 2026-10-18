#!/usr/bin/env bash
# Lint plan lifecycle metadata. See plans/README.md.
set -euo pipefail
cd "$(dirname "$0")/.."

valid="proposed in-progress implemented superseded historical"
fail=0
err() { echo "plans-lint: $*" >&2; fail=1; }
has() { [[ " $1 " == *" $2 "* ]]; }

for f in plans/*.md plans/archive/*.md; do
	[[ -e "$f" ]] || continue
	[[ "$f" == plans/README.md ]] && continue
	status=$(head -n 15 "$f" | sed -n 's/^Status: *\([a-z-]*\) *$/\1/p' | head -n 1)
	if [[ -z "$status" ]]; then
		err "$f: missing 'Status: <value>' line in first 15 lines (one of: $valid)"
		continue
	fi
	if ! has "$valid" "$status"; then
		err "$f: invalid status '$status' (one of: $valid)"
		continue
	fi
	case "$f" in
	plans/archive/*)
		if [[ "$status" == proposed || "$status" == in-progress ]]; then
			err "$f: $status plan must live in plans/, not plans/archive/"
		fi
		;;
	*)
		if [[ "$status" != proposed && "$status" != in-progress ]]; then
			err "$f: $status plan should be moved to plans/archive/"
		fi
		;;
	esac
	# Links of the form plans/<name>.md or plans/archive/<name>.md must resolve.
	while IFS= read -r link; do
		[[ -e "$link" ]] || err "$f: broken plan link '$link'"
	done < <(grep -o -E 'plans/(archive/)?[A-Za-z0-9_.-]+\.md' "$f" | sort -u)
done

exit "$fail"
