#!/bin/sh
# Regenerates *.args / *.out goldens from the REAL upstream GrepTool.Execute
# at commit fa7aed75 (never from HEAD). Run from the repo root:
#   internal/tools/testdata/grep/regenerate.sh
set -eu
REV=fa7aed75
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
scratch="$repo/tmp/upstream-$REV"
rm -rf "$scratch"; mkdir -p "$scratch"
git -C "$repo" archive "$REV" | tar -x -C "$scratch"
# Ignored build assets that some imports need (embedded UI); copy from the worktree.
for d in internal/serveui/static/dist internal/uisource/assets; do
  if [ -d "$repo/$d" ]; then mkdir -p "$scratch/$d"; cp -R "$repo/$d/." "$scratch/$d/"; fi
done
cp "$here/gen_golden_test.go.txt" "$scratch/internal/tools/zz_golden_gen_test.go"
(cd "$scratch" && GOLDEN_FIXTURES="$here" GOLDEN_UPSTREAM_REV="$REV" scripts/gotest.sh ./internal/tools -run '^TestGenerateGrepGolden$' -count=1)
echo "generated from $(git -C "$repo" rev-parse "$REV")"
