#!/bin/sh
set -eu

# Keep CI/runtime bounded while still entering Go's fuzzing phase (not only the
# committed seed corpus). Override these durations for longer local stress runs.
differential_time=${FUZZ_DIFFERENTIAL_TIME:-30s}
shift_time=${FUZZ_SHIFT_TIME:-15s}
row_parse_time=${FUZZ_ROW_PARSE_TIME:-10s}
row_predicate_time=${FUZZ_ROW_PREDICATE_TIME:-10s}
cell_shift_time=${FUZZ_CELL_SHIFT_TIME:-10s}
parse_time=${FUZZ_PARSE_TIME:-15s}
decode_loop_time=${FUZZ_DECODE_LOOP_TIME:-15s}
decode_overread_time=${FUZZ_DECODE_OVERREAD_TIME:-15s}
decode_introducer_time=${FUZZ_DECODE_INTRODUCER_TIME:-15s}

runtime_dir="$(dirname "$0")/../internal/terminal/runtime"
renderer_dir="$(dirname "$0")/../internal/terminal/renderer"

(cd "$runtime_dir" && go test -run '^$' -fuzz '^FuzzIncrementalRendererMatchesForcedFullRedraw$' -fuzztime="$differential_time" .)
(cd "$runtime_dir" && go test -run '^$' -fuzz '^FuzzDetectContentShiftExactOverlap$' -fuzztime="$shift_time" .)
(cd "$runtime_dir" && go test -run '^$' -fuzz '^FuzzScrollLinesIndependentImpliesEquivalentRowParsing$' -fuzztime="$row_parse_time" .)
(cd "$runtime_dir" && go test -run '^$' -fuzz '^FuzzLineScrollIndependentMatchesReference$' -fuzztime="$row_predicate_time" .)
(cd "$runtime_dir" && go test -run '^$' -fuzz '^FuzzShiftCellbufRegionMatchesRotation$' -fuzztime="$cell_shift_time" .)
(cd "$renderer_dir" && go test -run '^$' -fuzz '^FuzzParseSequence$' -fuzztime="$parse_time" .)
(cd "$renderer_dir" && go test -run '^$' -fuzz '^FuzzDecodeConsumesInputWithinBounds$' -fuzztime="$decode_loop_time" .)
(cd "$renderer_dir" && go test -run '^$' -fuzz '^FuzzDecodeIgnoresBytesBeyondLength$' -fuzztime="$decode_overread_time" .)
(cd "$renderer_dir" && go test -run '^$' -fuzz '^FuzzDecodeC1IntroducerMatchesEscForm$' -fuzztime="$decode_introducer_time" .)
