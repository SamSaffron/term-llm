package ui

import "testing"

func TestFormatToolPhaseSanitizesTerminalControls(t *testing.T) {
	phase := FormatToolPhase("shell\x1b[2J", "(echo)\nspoofed\x07")
	if want := "shell(echo) spoofed"; phase.Active != want || phase.Completed != want {
		t.Fatalf("FormatToolPhase() = %#v, want %q", phase, want)
	}
}
