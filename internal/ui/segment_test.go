package ui

import "testing"

func TestSafeANSISlice(t *testing.T) {
	tests := []struct {
		name string
		s    string
		pos  int
		want string
	}{
		{
			name: "no escape sequences",
			s:    "hello world",
			pos:  6,
			want: "world",
		},
		{
			name: "pos at zero",
			s:    "hello world",
			pos:  0,
			want: "hello world",
		},
		{
			name: "pos negative",
			s:    "hello world",
			pos:  -1,
			want: "hello world",
		},
		{
			name: "pos at end",
			s:    "hello",
			pos:  5,
			want: "",
		},
		{
			name: "pos past end",
			s:    "hello",
			pos:  10,
			want: "",
		},
		{
			name: "slice after complete escape sequence",
			s:    "\033[38;2;255;165;0mtext",
			pos:  17, // right after 'm', should work fine
			want: "text",
		},
		{
			name: "slice mid-escape loses CSI prefix",
			// Position 2 is after ESC[ but before terminator - should skip to after 'm'
			s:    "\033[38;2;255;165;0mclaimed_by",
			pos:  2, // mid-sequence
			want: "claimed_by",
		},
		{
			name: "slice mid-escape at parameter",
			// ESC [ 3 8 ; 2 ; 2 5 5 ; 1 6 5 ; 0 m
			// 0   1 2 3 4 5 6 7 8 9 ...
			s:    "\033[38;2;255;165;0mclaimed",
			pos:  5, // at ';2' part
			want: "claimed",
		},
		{
			name: "multiple sequences safe slice",
			s:    "\033[1mhello\033[0m world",
			pos:  4, // at 'h', just after the 'm' terminator
			want: "hello\033[0m world",
		},
		{
			name: "slice at start of second sequence",
			s:    "\033[1mhi\033[0m there",
			pos:  6, // at ESC of second sequence - this is a safe slice point
			want: "\033[0m there",
		},
		{
			name: "slice between sequences at text",
			s:    "\033[1mhi\033[0m there",
			pos:  5, // at 'i' between sequences
			want: "i\033[0m there",
		},
		{
			name: "empty string",
			s:    "",
			pos:  0,
			want: "",
		},
		{
			name: "SGR reset sequence",
			s:    "text\033[0mmore",
			pos:  4, // at ESC start
			want: "\033[0mmore",
		},
		{
			name: "slice right after ESC",
			s:    "\033[31mred",
			pos:  1, // after ESC, at '['
			want: "red",
		},
		{
			name: "24-bit RGB color mid-slice",
			// This reproduces the exact bug: \033[38;2;255;165;0m sliced partway produces "7;38mclaimed"
			s:    "prefix\033[38;2;255;165;0mclaimed_by",
			pos:  12, // somewhere in the middle of the escape sequence
			want: "claimed_by",
		},
		{
			name: "long sequence exceeding 20 bytes",
			// A very long SGR sequence with multiple parameters (>20 bytes)
			// ESC [ 38;2;255;165;0;48;2;0;0;0;1;4 m = 34 bytes before 'm'
			s:    "\033[38;2;255;165;0;48;2;0;0;0;1;4mtext",
			pos:  25, // mid-sequence, well past 20 bytes from ESC
			want: "text",
		},
		{
			name: "non-CSI sequence not adjusted",
			// ESC 7 is cursor save (not CSI) - we don't parse it, just slice at pos
			s:    "\0337saved\033[0m",
			pos:  1, // after ESC, at '7'
			want: "7saved\033[0m",
		},
		{
			name: "OSC sequence not adjusted",
			// ESC ] is OSC introducer (not CSI) - we don't parse it, just slice at pos
			// bytes: ESC(0) ](1) 0(2) ;(3) t(4) i(5) t(6) l(7) e(8) BEL(9) t(10) e(11) x(12) t(13)
			s:    "\033]0;title\007text",
			pos:  4, // at 't' of 'title', mid-OSC
			want: "title\007text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeANSISlice(tt.s, tt.pos)
			if got != tt.want {
				t.Errorf("safeANSISlice(%q, %d) = %q, want %q", tt.s, tt.pos, got, tt.want)
			}
		})
	}
}

func TestExtractAgentName(t *testing.T) {
	tests := []struct {
		name     string
		toolInfo string
		want     string
	}{
		{
			name:     "empty",
			toolInfo: "",
			want:     "",
		},
		{
			name:     "with parens and @",
			toolInfo: "(@reviewer: Analyze the codebase...)",
			want:     "reviewer",
		},
		{
			name:     "with @ no parens",
			toolInfo: "@reviewer: Analyze the codebase...",
			want:     "reviewer",
		},
		{
			name:     "no @ or parens",
			toolInfo: "reviewer: Analyze the codebase...",
			want:     "reviewer",
		},
		{
			name:     "just name",
			toolInfo: "reviewer",
			want:     "reviewer",
		},
		{
			name:     "name with space",
			toolInfo: "reviewer some prompt",
			want:     "reviewer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAgentName(tt.toolInfo)
			if got != tt.want {
				t.Errorf("extractAgentName(%q) = %q, want %q", tt.toolInfo, got, tt.want)
			}
		})
	}
}

func TestSegmentSeparator_CompactEverywhere(t *testing.T) {
	cases := []struct {
		name string
		prev SegmentType
		curr SegmentType
		want string
	}{
		{name: "text to text", prev: SegmentText, curr: SegmentText, want: ""},
		{name: "text to tool", prev: SegmentText, curr: SegmentTool, want: "\n"},
		{name: "text to ask_user", prev: SegmentText, curr: SegmentAskUserResult, want: "\n"},
		{name: "text to image", prev: SegmentText, curr: SegmentImage, want: "\n"},
		{name: "text to diff", prev: SegmentText, curr: SegmentDiff, want: "\n"},
		{name: "tool to text", prev: SegmentTool, curr: SegmentText, want: "\n\n"},
		{name: "tool to image", prev: SegmentTool, curr: SegmentImage, want: "\n"},
		{name: "tool to diff", prev: SegmentTool, curr: SegmentDiff, want: "\n"},
		{name: "image to tool", prev: SegmentImage, curr: SegmentTool, want: "\n"},
		{name: "image to text", prev: SegmentImage, curr: SegmentText, want: "\n"},
		{name: "diff to text", prev: SegmentDiff, curr: SegmentText, want: "\n"},
		{name: "diff to ask_user", prev: SegmentDiff, curr: SegmentAskUserResult, want: "\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SegmentSeparator(tc.prev, tc.curr); got != tc.want {
				t.Fatalf("SegmentSeparator(%v, %v) = %q, want %q", tc.prev, tc.curr, got, tc.want)
			}
		})
	}
}
