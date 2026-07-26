#!/bin/sh
set -eu

# Keep CI/runtime bounded while still entering Go's fuzzing phase (not only the
# committed seed corpus). Override these durations for longer local stress runs.
differential_time=${FUZZ_DIFFERENTIAL_TIME:-30s}
shift_time=${FUZZ_SHIFT_TIME:-15s}

cd "$(dirname "$0")/../third_party/bubbletea"
go test -run '^$' -fuzz '^FuzzIncrementalRendererMatchesForcedFullRedraw$' -fuzztime="$differential_time" .
go test -run '^$' -fuzz '^FuzzDetectContentShiftExactOverlap$' -fuzztime="$shift_time" .
