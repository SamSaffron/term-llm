package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestComposeFlushFirstCommands(t *testing.T) {
	cmdA := func() tea.Msg { return "a" }
	cmdB := func() tea.Msg { return "b" }

	tests := []struct {
		name        string
		flush       []tea.Cmd
		async       []tea.Cmd
		wantNil     bool
		wantBatch   bool
		wantMessage tea.Msg
	}{
		{
			name:    "no commands",
			flush:   nil,
			async:   nil,
			wantNil: true,
		},
		{
			name:        "single async command",
			flush:       nil,
			async:       []tea.Cmd{cmdA},
			wantMessage: "a",
		},
		{
			name:      "multiple async commands use batch",
			flush:     nil,
			async:     []tea.Cmd{cmdA, cmdB},
			wantBatch: true,
		},
		{
			name:        "single flush command",
			flush:       []tea.Cmd{cmdA},
			async:       nil,
			wantMessage: "a",
		},
		{
			name:      "flush and async uses sequence not batch",
			flush:     []tea.Cmd{cmdA},
			async:     []tea.Cmd{cmdB},
			wantBatch: false,
		},
		{
			name:      "multiple flush commands use sequence",
			flush:     []tea.Cmd{cmdA, cmdB},
			async:     nil,
			wantBatch: false,
		},
		{
			name:      "flush nils are compacted",
			flush:     []tea.Cmd{nil, cmdA},
			async:     []tea.Cmd{nil, cmdB},
			wantBatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := ComposeFlushFirstCommands(tc.flush, tc.async)
			if tc.wantNil {
				if cmd != nil {
					t.Fatalf("expected nil command")
				}
				return
			}

			if cmd == nil {
				t.Fatalf("expected non-nil command")
			}

			msg := cmd()
			_, isBatch := msg.(tea.BatchMsg)
			if isBatch != tc.wantBatch {
				t.Fatalf("batch=%v, want %v", isBatch, tc.wantBatch)
			}

			if tc.wantMessage != nil && msg != tc.wantMessage {
				t.Fatalf("message=%v, want %v", msg, tc.wantMessage)
			}
		})
	}
}
