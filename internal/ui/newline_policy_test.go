package ui

import "testing"

func TestCountTrailingNewlines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "empty", in: "", want: 0},
		{name: "none", in: "abc", want: 0},
		{name: "one", in: "abc\n", want: 1},
		{name: "two", in: "abc\n\n", want: 2},
		{name: "three", in: "abc\n\n\n", want: 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountTrailingNewlines(tc.in); got != tc.want {
				t.Fatalf("CountTrailingNewlines(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewlinePadding(t *testing.T) {
	tests := []struct {
		name    string
		current int
		target  int
		want    string
	}{
		{name: "already enough", current: 2, target: 2, want: ""},
		{name: "more than enough", current: 3, target: 2, want: ""},
		{name: "needs one", current: 1, target: 2, want: "\n"},
		{name: "needs two", current: 0, target: 2, want: "\n\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewlinePadding(tc.current, tc.target); got != tc.want {
				t.Fatalf("NewlinePadding(%d, %d) = %q, want %q", tc.current, tc.target, got, tc.want)
			}
		})
	}
}

func TestScrollbackPrintlnCommands_FinalSpacerPolicy(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		withSpacer  bool
		wantCmdsLen int
	}{
		{
			name:        "no content with spacer",
			content:     "",
			withSpacer:  true,
			wantCmdsLen: 1,
		},
		{
			name:        "no content without spacer",
			content:     "",
			withSpacer:  false,
			wantCmdsLen: 0,
		},
		{
			name:        "content without trailing newline needs extra spacer",
			content:     "hello",
			withSpacer:  true,
			wantCmdsLen: 2,
		},
		{
			name:        "content with one trailing newline already has spacer after println",
			content:     "hello\n",
			withSpacer:  true,
			wantCmdsLen: 1,
		},
		{
			name:        "content with two trailing newlines preserves markdown spacing",
			content:     "hello\n\n",
			withSpacer:  true,
			wantCmdsLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmds := ScrollbackPrintlnCommands(tc.content, tc.withSpacer)
			if got := len(cmds); got != tc.wantCmdsLen {
				t.Fatalf("ScrollbackPrintlnCommands(%q, %v) len = %d, want %d", tc.content, tc.withSpacer, got, tc.wantCmdsLen)
			}
		})
	}
}
