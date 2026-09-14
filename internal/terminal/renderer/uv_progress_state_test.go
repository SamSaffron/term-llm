package uv

import "testing"

// TestProgressBarStateStringOutOfRange is a red-test-first regression test:
// ProgressBarState.String() must not panic for out-of-range values and must
// return a stable fallback string.
func TestProgressBarStateStringOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		state ProgressBarState
		want  string
	}{
		{ProgressBarNone, "None"},
		{ProgressBarDefault, "Default"},
		{ProgressBarError, "Error"},
		{ProgressBarIndeterminate, "Indeterminate"},
		{ProgressBarWarning, "Warning"},
		{ProgressBarState(-1), "Unknown"},
		{ProgressBarState(99), "Unknown"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("ProgressBarState(%d).String() = %q, want %q", int(tc.state), got, tc.want)
		}
	}
}
