package testutil

import (
	"bytes"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func writeContaining(writes [][]byte, marker []byte, start int) int {
	for i := start; i < len(writes); i++ {
		if bytes.Contains(writes[i], marker) {
			return i
		}
	}
	return -1
}

func TestTerminalOutputHarnessCapturesWriteBoundaries(t *testing.T) {
	harness := NewTerminalOutputHarness()
	if err := harness.RunCommands(tea.Println("first"), tea.Println("second")); err != nil {
		t.Fatalf("RunCommands() error = %v", err)
	}

	writes := harness.Writes()
	firstWrite := writeContaining(writes, []byte("first"), 0)
	secondWrite := writeContaining(writes, []byte("second"), 0)
	if firstWrite < 0 || secondWrite < 0 {
		t.Fatalf("captured writes do not contain both lines: %q", writes)
	}
	if firstWrite >= secondWrite {
		t.Fatalf("line write order first=%d second=%d in %q", firstWrite, secondWrite, writes)
	}
	output := harness.Bytes()
	if bytes.Count(output, []byte("first")) != 1 || bytes.Count(output, []byte("second")) != 1 {
		t.Fatalf("captured output duplicated lines: %q", output)
	}

	writes[firstWrite][0] = 'x'
	if got := harness.Writes()[firstWrite][0]; got == 'x' {
		t.Fatal("Writes() exposed mutable capture storage")
	}
}
