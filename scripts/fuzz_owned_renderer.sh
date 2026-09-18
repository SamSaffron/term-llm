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
styled_draw_time=${FUZZ_STYLED_DRAW_TIME:-15s}

runtime_dir="$(dirname "$0")/../internal/terminal/runtime"
renderer_dir="$(dirname "$0")/../internal/terminal/renderer"

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

# Go's fuzzing coordinator sometimes reports a bare "context deadline exceeded"
# when -fuzztime expires while it is shutting its workers down. That failure
# writes no failing input, reproduces on unrelated targets, and is independent
# of the code under test, so retry such a target once. Real findings always name
# the input they wrote or the seed entry that failed, so they still stop the run
# on the first attempt.
fuzz_target() {
	module_dir=$1
	target=$2
	duration=$3
	attempt=1
	log="$work_dir/fuzz.log"
	status="$work_dir/fuzz.status"
	while :; do
		echo 1 >"$status"
		(
			cd "$module_dir" || exit 1
			set +e
			go test -run '^$' -fuzz "^${target}\$" -fuzztime="$duration" .
			echo $? >"$status"
		) 2>&1 | tee "$log"
		if [ "$(cat "$status")" -eq 0 ]; then
			return 0
		fi
		if [ "$attempt" -ge 2 ] ||
			! grep -q '^[[:space:]]*context deadline exceeded$' "$log" ||
			grep -q 'Failing input written to' "$log" ||
			grep -q 'failure while testing seed corpus entry' "$log"; then
			return 1
		fi
		echo "note: $target hit Go's fuzzing shutdown deadline without a failing input; retrying once"
		attempt=$((attempt + 1))
	done
}

fuzz_target "$runtime_dir" FuzzIncrementalRendererMatchesForcedFullRedraw "$differential_time"
fuzz_target "$runtime_dir" FuzzDetectContentShiftExactOverlap "$shift_time"
fuzz_target "$runtime_dir" FuzzScrollLinesIndependentImpliesEquivalentRowParsing "$row_parse_time"
fuzz_target "$runtime_dir" FuzzLineScrollIndependentMatchesReference "$row_predicate_time"
fuzz_target "$runtime_dir" FuzzShiftCellbufRegionMatchesRotation "$cell_shift_time"
fuzz_target "$renderer_dir" FuzzParseSequence "$parse_time"
fuzz_target "$renderer_dir" FuzzDecodeConsumesInputWithinBounds "$decode_loop_time"
fuzz_target "$renderer_dir" FuzzDecodeIgnoresBytesBeyondLength "$decode_overread_time"
fuzz_target "$renderer_dir" FuzzDecodeC1IntroducerMatchesEscForm "$decode_introducer_time"
fuzz_target "$renderer_dir" FuzzStyledStringDrawStaysInBounds "$styled_draw_time"
