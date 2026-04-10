package ui

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/ansi"
	"github.com/muesli/reflow/dedent"
	"github.com/muesli/reflow/margin"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"
)

// --- ansi.PrintableRuneWidth ---

func TestPrintableRuneWidthCSI(t *testing.T) {
	s := "\x1b[31mhello\x1b[0m"
	got := ansi.PrintableRuneWidth(s)
	if got != 5 {
		t.Errorf("PrintableRuneWidth(CSI) = %d, want 5", got)
	}
}

func TestPrintableRuneWidthOSC8(t *testing.T) {
	s := "\x1b]8;;https://example.com\x07Link Text\x1b]8;;\x07"
	got := ansi.PrintableRuneWidth(s)
	if got != 9 {
		t.Errorf("PrintableRuneWidth(OSC8) = %d, want 9", got)
	}
}

func TestPrintableRuneWidthOSCWithST(t *testing.T) {
	s := "\x1b]0;window title\x1b\\visible"
	got := ansi.PrintableRuneWidth(s)
	if got != 7 {
		t.Errorf("PrintableRuneWidth(OSC+ST) = %d, want 7", got)
	}
}

func TestPrintableRuneWidthDCS(t *testing.T) {
	s := "\x1bPsome;dcs;data\x1b\\visible"
	got := ansi.PrintableRuneWidth(s)
	if got != 7 {
		t.Errorf("PrintableRuneWidth(DCS) = %d, want 7", got)
	}
}

func TestPrintableRuneWidthMixed(t *testing.T) {
	s := "\x1b[31m\x1b]8;;http://x.com\x07link\x1b]8;;\x07 plain\x1b[0m"
	got := ansi.PrintableRuneWidth(s)
	if got != 10 { // "link" + " plain"
		t.Errorf("PrintableRuneWidth(mixed) = %d, want 10", got)
	}
}

func TestPrintableRuneWidthCSINonLetterFinalByte(t *testing.T) {
	// CSI final bytes include ~ (0x7E), used by e.g. bracketed paste \x1b[200~
	s := "\x1b[200~hello\x1b[201~"
	got := ansi.PrintableRuneWidth(s)
	if got != 5 { // "hello"
		t.Errorf("PrintableRuneWidth(CSI with ~) = %d, want 5", got)
	}
}

// --- wordwrap ---

func TestWordwrapBreakpointCountsInLineLen(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
	}{
		{"hyphenated_words", "aaa bbb ccc ddd eee fff ggg hhh iii jjj kkk lll mmm nnn ooo ppp-qqq rrr sss ttt uuu vvv www xxxx YYYY and more text after.", 100},
		{"multiple_hyphens", "anthropic-beta context-window rate-limit-2025 some more words to fill the line up to the limit here.", 80},
		{"small_limit", "aaa-bbb cccc dddd eeee ffff gggg hhhh.", 20},
		{"boundary_exact_limit", strings.Repeat("a", 10) + "-b", 10},
		{"boundary_limit_minus_one", strings.Repeat("a", 9) + "-b", 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := wordwrap.String(tc.text, tc.limit)
			for i, line := range strings.Split(result, "\n") {
				w := xansi.StringWidth(line)
				if w > tc.limit {
					t.Errorf("line %d exceeds limit (width %d, limit %d): %q", i, w, tc.limit, line)
				}
			}
		})
	}
}

func TestWordwrapMultipleWrites(t *testing.T) {
	w := wordwrap.NewWriter(20)
	_, _ = w.Write([]byte("hello-world "))
	_, _ = w.Write([]byte("foo-bar baz"))
	_ = w.Close()
	for i, line := range strings.Split(w.String(), "\n") {
		if xansi.StringWidth(line) > 20 {
			t.Errorf("line %d exceeds limit: %q", i, line)
		}
	}
}

func TestWordwrapANSIRestoredAtLinebreak(t *testing.T) {
	result := wordwrap.String("\x1b[31mhello world\x1b[0m", 7)
	lines := strings.Split(result, "\n")
	if len(lines) < 2 {
		t.Skip("text did not wrap")
	}
	for i, line := range lines {
		if len(line) > 0 && !strings.Contains(line, "\x1b[31m") {
			t.Errorf("line %d missing color restore: %q", i, line)
		}
	}
	if !strings.HasSuffix(lines[0], "\x1b[0m") {
		t.Errorf("line 0 missing trailing reset: %q", lines[0])
	}
}

func TestWordwrapPlainTextNoANSI(t *testing.T) {
	result := wordwrap.String("hello world", 7)
	if strings.Contains(result, "\x1b") {
		t.Errorf("plain text should have no ANSI sequences: %q", result)
	}
}

func TestWordwrapOSC8Preserved(t *testing.T) {
	// OSC8 hyperlink wrapping: link text should wrap correctly, URL must not be split.
	link := "\x1b]8;;https://example.com\x07Click Here\x1b]8;;\x07 and more text after"
	result := wordwrap.String(link, 15)
	if !strings.Contains(result, "https://example.com") {
		t.Errorf("URL was corrupted: %q", result)
	}
	// "Click Here" (10 chars) should stay together on one line.
	for i, line := range strings.Split(result, "\n") {
		if xansi.StringWidth(line) > 15 {
			t.Errorf("line %d exceeds limit: %q", i, line)
		}
	}
}

// --- wrap ---

func TestWrapFastPathNormalizesContent(t *testing.T) {
	w := wrap.NewWriter(80)
	_, _ = w.Write([]byte("a\tb"))
	if got := w.String(); got != "a    b" {
		t.Errorf("got %q, want %q", got, "a    b")
	}
}

func TestWrapFastPathKeepNewlinesFalse(t *testing.T) {
	w := wrap.NewWriter(80)
	w.KeepNewlines = false
	_, _ = w.Write([]byte("hello\nworld"))
	if strings.Contains(w.String(), "\n") {
		t.Errorf("fast path did not strip newlines: %q", w.String())
	}
}

func TestWrapLineLenResetOnEmbeddedNewline(t *testing.T) {
	w := wrap.NewWriter(10)
	_, _ = w.Write([]byte("a\nb"))
	_, _ = w.Write([]byte(strings.Repeat("x", 9)))
	lines := strings.Split(w.String(), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d: %q", len(lines), w.String())
	}
	if len(lines) == 2 && xansi.StringWidth(lines[1]) != 10 {
		t.Errorf("second line width = %d, want 10", xansi.StringWidth(lines[1]))
	}
}

func TestWrapNoLineExceedsLimit(t *testing.T) {
	tests := []struct {
		name  string
		input string
		limit int
	}{
		{"simple", "hello world this is a test of wrapping behavior", 20},
		{"with_tabs", "col1\tcol2\tcol3\tcol4", 15},
		{"long_word", strings.Repeat("a", 30), 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i, line := range strings.Split(wrap.String(tc.input, tc.limit), "\n") {
				if w := xansi.StringWidth(line); w > tc.limit {
					t.Errorf("line %d exceeds limit (width %d, limit %d): %q", i, w, tc.limit, line)
				}
			}
		})
	}
}

func TestWrapOSC8NotBroken(t *testing.T) {
	link := "\x1b]8;;https://example.com\x07Click\x1b]8;;\x07 more"
	result := wrap.String(link, 20)
	if !strings.Contains(result, "https://example.com") {
		t.Errorf("OSC8 URL was corrupted: %q", result)
	}
}

// --- truncate ---

func TestTruncateBasic(t *testing.T) {
	if got := truncate.String("hello world", 5); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTruncateWithTail(t *testing.T) {
	if got := truncate.StringWithTail("hello world", 8, "..."); got != "hello..." {
		t.Errorf("got %q, want %q", got, "hello...")
	}
}

func TestTruncateExactFitPreserved(t *testing.T) {
	// Content that fits exactly should not be truncated.
	if got := truncate.StringWithTail("foobar", 6, "."); got != "foobar" {
		t.Errorf("got %q, want %q", got, "foobar")
	}
	if got := truncate.StringWithTail("foo", 6, "..."); got != "foo" {
		t.Errorf("got %q, want %q", got, "foo")
	}
}

func TestTruncateMultipleWrites(t *testing.T) {
	tests := []struct {
		name   string
		width  uint
		tail   string
		writes []string
		want   string
	}{
		{"cumulative_no_tail", 8, "", []string{"hel", "lo world"}, "hello wo"},
		{"cumulative_with_tail", 8, "...", []string{"hel", "lo world"}, "hello..."},
		{"exact_fit", 6, ".", []string{"foo", "bar"}, "foobar"},
		{"overflow", 6, ".", []string{"foo", "barbaz"}, "fooba."},
		{"width_stable", 10, "...", []string{"ab", "cd", "ef"}, "abcdef"},
		{"post_truncation_ignored", 5, "", []string{"hello world", " more"}, "hello"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := truncate.NewWriter(tc.width, tc.tail)
			for _, s := range tc.writes {
				_, _ = w.Write([]byte(s))
			}
			if got := w.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncateOSC8NotCounted(t *testing.T) {
	// OSC8 hyperlink URL should not count toward truncation width.
	s := "\x1b]8;;https://example.com\x07Link\x1b]8;;\x07"
	got := truncate.String(s, 4) // "Link" is 4 chars — should fit exactly
	if !strings.Contains(got, "Link") {
		t.Errorf("OSC8 link text was truncated: %q", got)
	}
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("OSC8 URL was corrupted: %q", got)
	}
}

func TestTruncateOSC8ClosedOnTruncation(t *testing.T) {
	// When truncation cuts inside an OSC8 hyperlink, the close sequence
	// must be emitted so the hyperlink doesn't leak into subsequent output.
	s := "\x1b]8;;https://example.com\x07Link Text\x1b]8;;\x07"
	got := truncate.StringWithTail(s, 4, ".")
	// "Link" fits (4), then " " overflows → truncate to "Lin" + "."
	// Must contain the OSC8 close sequence.
	if !strings.Contains(got, "\x1b]8;;\x07") {
		t.Errorf("truncated OSC8 hyperlink missing close sequence: %q", got)
	}
}

// --- margin ---

func TestMarginBasic(t *testing.T) {
	if got := margin.String("hello", 15, 2); got != "  hello        " {
		t.Errorf("got %q, want %q", got, "  hello        ")
	}
}

func TestMarginMultipleWritesNoDuplication(t *testing.T) {
	w := margin.NewWriter(20, 2, nil)
	_, _ = w.Write([]byte("line1\n"))
	_, _ = w.Write([]byte("line2\n"))
	_ = w.Close()
	got := w.String()
	if c := strings.Count(got, "line1"); c != 1 {
		t.Errorf("line1 appears %d times in: %q", c, got)
	}
	if c := strings.Count(got, "line2"); c != 1 {
		t.Errorf("line2 appears %d times in: %q", c, got)
	}
}

func TestMarginMultilineContent(t *testing.T) {
	if got := margin.String("a\nb", 10, 1); got != " a        \n b        " {
		t.Errorf("got %q, want %q", got, " a        \\n b        ")
	}
}

// --- dedent ---

func TestDedentPreservesInternalSpaces(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"mixed_indent", "no indent here\n  indented line", "no indent here\nindented line"},
		{"multiple_lines", "first line\n  second line\n  third line", "first line\nsecond line\nthird line"},
		{"all_indented", "  hello world\n    indented more", "hello world\n  indented more"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dedent.String(tc.input); got != tc.want {
				t.Errorf("dedent(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
