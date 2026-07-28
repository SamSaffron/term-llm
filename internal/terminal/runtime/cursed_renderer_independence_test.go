package tea

import (
	"io"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// referenceLineScrollIndependent is a deliberately plain transcription of the
// row-independence rules. lineScrollIndependent classifies each byte in a
// different order so the common printable-ASCII case costs one comparison;
// this reference exists purely to prove the two agree.
func referenceLineScrollIndependent(line string) bool {
	hasSGR := false
	for i := 0; i < len(line); i++ {
		if line[i] != '\x1b' {
			if line[i] < 0x20 || line[i] == 0x7f {
				return false
			}
			if line[i] >= utf8.RuneSelf {
				r, size := utf8.DecodeRuneInString(line[i:])
				if unicode.IsControl(r) || unicode.IsMark(r) || unicode.Is(unicode.Cf, r) ||
					(!unicode.IsGraphic(r) && !unicode.Is(unicode.Co, r)) {
					return false
				}
				i += size - 1
			}
			continue
		}
		hasSGR = true
		if i+2 >= len(line) || line[i+1] != '[' {
			return false
		}
		i += 2
		for i < len(line) {
			b := line[i]
			if (b < '0' || b > '9') && b != ';' && b != ':' {
				break
			}
			i++
		}
		if i >= len(line) || line[i] != 'm' {
			return false
		}
	}
	if hasSGR && !strings.HasSuffix(line, "\x1b[0m") && !strings.HasSuffix(line, "\x1b[m") {
		return false
	}
	return true
}

// FuzzLineScrollIndependentMatchesReference proves the byte classification used
// by the hot predicate accepts exactly the same rows as the plain transcription
// of its rules. The predicate gates the incremental redraw paths, so widening
// it by even one byte class would let unsafe rows be parsed in isolation.
func FuzzLineScrollIndependentMatchesReference(f *testing.F) {
	for _, seed := range []string{
		"",
		"plain text",
		"\x1b[31mred\x1b[0m",
		"\x1b[m",
		"\x1b[ m\x1b[m",
		"\x1b[?m\x1b[m",
		"\x1b[38;2;1;2;3mtrue colour\x1b[0m",
		"\x1b[4:3munderline\x1b[m",
		"tab\there",
		"\x7f",
		"\x1b]8;;https://example.com\x07link\x1b]8;;\x07",
		"界 wide",
		"éclair",
		"​zero width",
		" private use",
		"\x1b[31munterminated",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		if got, want := lineScrollIndependent(line), referenceLineScrollIndependent(line); got != want {
			t.Fatalf("lineScrollIndependent(%q) = %t, reference = %t", line, got, want)
		}
	})
}

func independenceTestFrame(height int, rows ...string) string {
	out := make([]string, height)
	copy(out, rows)
	return strings.Join(out, "\n")
}

// TestRetainedRowIndependenceMemoMatchesRecompute pins the memo that lets a
// flush reuse the row-independence verdict already proven for the retained
// frame instead of rescanning it. A stale verdict would admit an unsafe frame
// to the incremental row paths, so every accepted frame must leave the memo
// equal to a fresh recomputation over the retained rows.
func TestRetainedRowIndependenceMemoMatchesRecompute(t *testing.T) {
	const width, height = 40, 10

	independent := independenceTestFrame(height, "alpha", "beta", "gamma", "delta")
	styled := independenceTestFrame(height, "\x1b[31malpha\x1b[0m", "beta", "\x1b[1;4mgamma\x1b[0m")
	linked := independenceTestFrame(height, "alpha", "\x1b]8;;https://example.com\x07beta\x1b]8;;\x07", "gamma")
	tabbed := independenceTestFrame(height, "alpha", "beta\tcolumn", "gamma")
	combining := independenceTestFrame(height, "alpha", "éclair", "gamma")
	unterminated := independenceTestFrame(height, "alpha", "\x1b[31mbeta", "gamma")
	oneRowChanged := independenceTestFrame(height, "alpha", "beta", "gamma-changed", "delta")

	r := newCursedRenderer(io.Discard, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)

	check := func(label string) {
		t.Helper()
		want := scrollLinesIndependent(r.lastContentLines)
		if r.lastContentIndependent != want {
			t.Fatalf("%s: retained independence memo = %t, want %t", label, r.lastContentIndependent, want)
		}
	}
	push := func(label, content string, altScreen bool) {
		t.Helper()
		view := NewView(content)
		view.AltScreen = altScreen
		r.render(view)
		if err := r.flush(false); err != nil {
			t.Fatalf("%s: flush: %v", label, err)
		}
		check(label)
	}

	// Alternate between safe and unsafe frames so the memo has to flip in both
	// directions, and repeat frames so the unchanged-row shortcut is exercised
	// from both a true and a false retained verdict.
	push("independent", independent, true)
	push("one row changed", oneRowChanged, true)
	push("styled", styled, true)
	push("linked", linked, true)
	push("linked again", linked, true)
	push("independent after link", independent, true)
	push("tabbed", tabbed, true)
	push("combining", combining, true)
	push("unterminated sgr", unterminated, true)
	push("styled after unterminated", styled, true)

	// Inline frames retain no content lines; the memo must follow.
	push("inline", independent, false)
	push("alt screen again", independent, true)

	// Invalidation and resize both drop the retained frame.
	r.mu.Lock()
	r.invalidateIncrementalLocked()
	r.mu.Unlock()
	check("after invalidate")
	push("after invalidate redraw", styled, true)

	r.resize(width+8, height+2)
	push("after resize", styled, true)
	push("after resize repeat", linked, true)
}

// TestContentLineBuffersDoNotShareStorage guards the precondition the memo
// relies on. A flush splits the incoming frame into the spare line buffer and
// then compares those rows against the retained ones; if the two buffers ever
// shared storage, every row would compare equal to itself and unsafe rows
// would be skipped instead of scanned.
func TestContentLineBuffersDoNotShareStorage(t *testing.T) {
	const width, height = 40, 10

	r := newCursedRenderer(io.Discard, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)
	for i, content := range []string{
		independenceTestFrame(height, "alpha", "beta", "gamma"),
		independenceTestFrame(height, "alpha", "beta-changed", "gamma"),
		independenceTestFrame(height, "alpha", "beta-changed", "gamma-changed"),
	} {
		view := NewView(content)
		view.AltScreen = true
		r.render(view)
		if err := r.flush(false); err != nil {
			t.Fatalf("frame %d: flush: %v", i, err)
		}
	}

	if len(r.lastContentLines) == 0 {
		t.Fatal("renderer retained no content lines")
	}
	spare := r.nextContentLines[:cap(r.nextContentLines)]
	if len(spare) == 0 {
		t.Fatal("renderer kept no spare line buffer to reuse")
	}
	if &spare[0] == &r.lastContentLines[0] {
		t.Fatal("spare and retained line buffers share storage")
	}
}

// TestRowIndependenceShortcutAgreesWithFullScan checks the unchanged-row
// shortcut directly: reusing the retained verdict for rows that did not change
// must produce the same answer as scanning every row of the new frame.
func TestRowIndependenceShortcutAgreesWithFullScan(t *testing.T) {
	retained := []string{"alpha", "\x1b[31mbeta\x1b[0m", "gamma", "\x1b]8;;u\x07link\x1b]8;;\x07"}
	frames := [][]string{
		{"alpha", "\x1b[31mbeta\x1b[0m", "gamma", "\x1b]8;;u\x07link\x1b]8;;\x07"},
		{"alpha", "\x1b[31mbeta\x1b[0m", "gamma", "plain"},
		{"alpha", "beta\tcolumn", "gamma", "plain"},
		{"alpha", "\x1b[31mbeta", "gamma", "plain"},
		{"alpha"},
		{},
		{"alpha", "\x1b[31mbeta\x1b[0m", "gamma", "plain", "extrá"},
	}

	r := &cursedRenderer{}
	for _, retainedIndependent := range []bool{true, false} {
		for i, frame := range frames {
			r.lastContentLines = retained
			r.lastContentIndependent = retainedIndependent
			got := r.contentLinesIndependent(frame)
			want := scrollLinesIndependent(frame)
			// The shortcut may only be trusted when the retained verdict holds.
			if retainedIndependent != scrollLinesIndependent(retained) {
				continue
			}
			if got != want {
				t.Fatalf("frame %d (retained independent=%t): shortcut = %t, full scan = %t", i, retainedIndependent, got, want)
			}
		}
	}
}
