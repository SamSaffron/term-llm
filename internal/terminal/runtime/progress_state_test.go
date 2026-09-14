package tea

import "testing"

func TestProgressBarStateStringOutOfRange(t *testing.T) {
	for _, state := range []ProgressBarState{-1, 99} {
		if got := state.String(); got != "Unknown" {
			t.Errorf("ProgressBarState(%d).String() = %q, want %q", state, got, "Unknown")
		}
	}
}
