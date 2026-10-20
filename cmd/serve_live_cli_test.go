package cmd

import (
	"testing"
)

func TestLiveControlPlaneCompletionValues(t *testing.T) {
	got := configValueCompletions("live.control_plane", "")
	want := []string{"off", "agent", "classify"}
	if len(got) != len(want) {
		t.Fatalf("completions = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("completions = %v, want %v", got, want)
		}
	}
}
