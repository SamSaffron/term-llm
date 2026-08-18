package ui

import (
	"reflect"
	"testing"
)

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
		name       string
		content    string
		withSpacer bool
		wantBody   string
		wantCmds   int
	}{
		{
			name:       "no content with spacer",
			content:    "",
			withSpacer: true,
			wantBody:   "\n",
			wantCmds:   1,
		},
		{
			name:       "no content without spacer",
			content:    "",
			withSpacer: false,
			wantCmds:   0,
		},
		{
			name:       "content without trailing newline gets atomic spacer",
			content:    "hello",
			withSpacer: true,
			wantBody:   "hello\n",
			wantCmds:   1,
		},
		{
			name:       "content with one trailing newline already has spacer",
			content:    "hello\n",
			withSpacer: true,
			wantBody:   "hello\n",
			wantCmds:   1,
		},
		{
			name:       "content with two trailing newlines preserves markdown spacing",
			content:    "hello\n\n",
			withSpacer: true,
			wantBody:   "hello\n\n",
			wantCmds:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmds := ScrollbackPrintlnCommands(tc.content, tc.withSpacer)
			if got := len(cmds); got != tc.wantCmds {
				t.Fatalf("ScrollbackPrintlnCommands(%q, %v) len = %d, want %d", tc.content, tc.withSpacer, got, tc.wantCmds)
			}
			if tc.wantCmds == 0 {
				return
			}
			msg := reflect.ValueOf(cmds[0]())
			body := msg.FieldByName("messageBody")
			if !body.IsValid() {
				t.Fatalf("command returned %T, want Bubble Tea printLineMessage", cmds[0]())
			}
			if got := body.String(); got != tc.wantBody {
				t.Fatalf("print body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

func TestStreamingNewlineCompactor_CompactsAcrossChunks(t *testing.T) {
	c := NewStreamingNewlineCompactor(2)

	part1 := c.CompactChunk("hello\n\n\n")
	part2 := c.CompactChunk("\n\nworld")

	if part1 != "hello\n\n" {
		t.Fatalf("part1 = %q, want %q", part1, "hello\n\n")
	}
	if part2 != "world" {
		t.Fatalf("part2 = %q, want %q", part2, "world")
	}
}

func TestStreamingNewlineCompactor_ResetsRunOnText(t *testing.T) {
	c := NewStreamingNewlineCompactor(2)

	got := c.CompactChunk("a\n\n\nb\n\n\n")
	if got != "a\n\nb\n\n" {
		t.Fatalf("got %q, want %q", got, "a\n\nb\n\n")
	}
}
