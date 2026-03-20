package ui

import (
	"strings"
	"testing"

	"github.com/muesli/reflow/ansi"
	"github.com/muesli/reflow/dedent"
	"github.com/muesli/reflow/padding"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/reflow/wordwrap"
)

// --- Item 1: non-[0m SGR reset should not leak stale styles ---

func TestWordwrapNonFullResetSGR(t *testing.T) {
	// \x1b[39m resets foreground to default. After wrapping, the restored
	// style should not visually re-apply the red that was cancelled.
	// Input: red on, text, default fg, more text that wraps.
	text := "\x1b[31mhello\x1b[39m world wrap"
	result := wordwrap.String(text, 8)
	lines := strings.Split(result, "\n")
	if len(lines) < 2 {
		t.Skip("text did not wrap")
	}
	// The second line should not start with \x1b[31m without a following \x1b[39m.
	// Since lastseq accumulates both, the restore replays \x1b[31m\x1b[39m which
	// visually cancels. Check that the net visual effect is no stale red.
	line1 := lines[1]
	redIdx := strings.Index(line1, "\x1b[31m")
	defIdx := strings.Index(line1, "\x1b[39m")
	if redIdx >= 0 && (defIdx < 0 || defIdx < redIdx) {
		t.Errorf("line 1 has stale red without cancellation: %q", line1)
	}
}

// --- Item 3: uint underflow on w.width - w.tailWidth ---

func TestTruncateWidthSmallerThanTail(t *testing.T) {
	// width=2, tail="..." (tailWidth=3). Should not panic or produce garbage.
	got := truncate.StringWithTail("hello", 2, "...")
	// With width < tailWidth, the tail is emitted directly.
	if got != "..." {
		t.Errorf("got %q, want %q", got, "...")
	}
}

func TestTruncateWidthEqualsTail(t *testing.T) {
	// width=3, tail="..." (tailWidth=3). Content can't fit, only tail.
	got := truncate.StringWithTail("hello", 3, "...")
	if got != "..." {
		t.Errorf("got %q, want %q", got, "...")
	}
}

func TestTruncateWidthOneLargerThanTail(t *testing.T) {
	// width=4, tail="..." (tailWidth=3). One char of content fits.
	got := truncate.StringWithTail("hello", 4, "...")
	if got != "h..." {
		t.Errorf("got %q, want %q", got, "h...")
	}
}

// --- Item 4: padding width counting with wide (CJK) runes ---

func TestPaddingWidthCJK(t *testing.T) {
	// CJK characters are 2 cells wide. Padding should account for this.
	// "あ" is 2 cells. With padding width 10, we need 8 spaces.
	got := padding.String("あ", 10)
	want := "あ        " // 2 + 8 = 10
	if got != want {
		t.Errorf("got %q (len %d), want %q (len %d)", got, len(got), want, len(want))
	}
}

func TestPaddingWidthMixedASCIICJK(t *testing.T) {
	// "aあ" = 1 + 2 = 3 cells. Padding to 10 = 7 spaces.
	got := padding.String("aあ", 10)
	want := "aあ       " // 3 + 7 = 10
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- Item 7: dedent variable shadowing (minIndent func vs var) ---

func TestDedentCorrectMinIndent(t *testing.T) {
	// Verify the shadowed minIndent variable doesn't cause incorrect behavior.
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"varying_indent_levels",
			"    four\n  two\n      six",
			"  four\ntwo\n    six",
		},
		{
			"tabs_and_spaces",
			"\thello\n\t\tworld",
			"hello\n\tworld",
		},
		{
			"single_line_indented",
			"  hello",
			"hello", // single indented line: indent=2 is the minimum, stripped
		},
		{
			"no_indent",
			"hello\nworld",
			"hello\nworld",
		},
		{
			"empty_lines_between",
			"  hello\n\n  world",
			"hello\n\nworld",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dedent.String(tc.input); got != tc.want {
				t.Errorf("dedent(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// --- Item 2: truncation ordering (tail → OSC8 close → SGR reset) ---

func TestTruncateOrderingAfterOSC8(t *testing.T) {
	// Verify the output after truncation has correct ordering:
	// visible content + tail + OSC8 close + SGR reset
	s := "\x1b[31m\x1b]8;;https://x.com\x07Long Link Text Here\x1b]8;;\x07\x1b[0m"
	got := truncate.StringWithTail(s, 6, ".")
	// Should contain: some visible text, ".", OSC8 close, then SGR reset
	osc8CloseIdx := strings.Index(got, "\x1b]8;;\x07")
	resetIdx := strings.Index(got, "\x1b[0m")
	tailIdx := strings.Index(got, ".")
	if osc8CloseIdx < 0 {
		t.Errorf("missing OSC8 close in: %q", got)
	}
	if tailIdx >= 0 && osc8CloseIdx >= 0 && tailIdx > osc8CloseIdx {
		t.Errorf("tail after OSC8 close (tail=%d, osc8close=%d): %q", tailIdx, osc8CloseIdx, got)
	}
	if resetIdx >= 0 && osc8CloseIdx >= 0 && resetIdx < osc8CloseIdx {
		t.Errorf("SGR reset before OSC8 close (reset=%d, osc8close=%d): %q", resetIdx, osc8CloseIdx, got)
	}
}

// --- Bonus: PrintableRuneWidth with \x1b[200~ (item from prior round, verify still works) ---

func TestParserBracketedPaste(t *testing.T) {
	// Bracketed paste mode uses ~ as CSI final byte.
	var p ansi.Parser
	for _, c := range "\x1b[200~" {
		p.Feed(c)
	}
	if p.InSequence() {
		t.Error("parser stuck in sequence after \\x1b[200~")
	}
}
