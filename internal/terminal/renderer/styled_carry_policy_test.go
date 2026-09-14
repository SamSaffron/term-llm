package uv

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestStyledStringEscapeCarryPolicy(t *testing.T) {
	t.Run("unterminated APC is dropped", func(t *testing.T) {
		const input = "\x1b_Ga=d"
		buf := NewScreenBuffer(32, 1)
		NewStyledString(input).Draw(buf, buf.Bounds())

		if cell := buf.CellAt(0, 0); cell != nil && strings.Contains(cell.Content, input) {
			t.Fatalf("unterminated APC was carried in cell content %q", cell.Content)
		}
	})

	t.Run("OSC is dropped", func(t *testing.T) {
		const input = "\x1b]0;title\x1b\\"
		buf := NewScreenBuffer(32, 1)
		NewStyledString(input).Draw(buf, buf.Bounds())

		if cell := buf.CellAt(0, 0); cell != nil && strings.Contains(cell.Content, input) {
			t.Fatalf("OSC was carried in cell content %q", cell.Content)
		}
	})

	t.Run("OSC 8 hyperlink with semicolons survives a draw", func(t *testing.T) {
		const url = "https://example.com/a;b?c=1;2"
		input := "\x1b]8;;" + url + "\x1b\\link\x1b]8;;\x1b\\"
		buf := NewScreenBuffer(32, 1)
		NewStyledString(input).Draw(buf, buf.Bounds())

		cell := buf.CellAt(0, 0)
		if cell == nil || cell.Link.URL != url {
			t.Fatalf("cell link = %#v, want URL %q", cell, url)
		}
	})

	t.Run("OSC 8 hyperlink is retained as link metadata", func(t *testing.T) {
		const input = "\x1b]8;;https://x\x1b\\hi\x1b]8;;\x1b\\"
		buf := NewScreenBuffer(32, 1)
		NewStyledString(input).Draw(buf, buf.Bounds())

		for x, content := range []string{"h", "i"} {
			cell := buf.CellAt(x, 0)
			if cell == nil {
				t.Fatalf("cell %d is nil", x)
			}
			if cell.Content != content {
				t.Errorf("cell %d content = %q, want %q", x, cell.Content, content)
			}
			if cell.Link.URL != "https://x" {
				t.Errorf("cell %d link URL = %q, want %q", x, cell.Link.URL, "https://x")
			}
		}
	})

	t.Run("non-data CSI is dropped", func(t *testing.T) {
		const input = "\x1b[2A"
		buf := NewScreenBuffer(32, 1)
		NewStyledString(input).Draw(buf, buf.Bounds())

		if cell := buf.CellAt(0, 0); cell != nil && strings.Contains(cell.Content, input) {
			t.Fatalf("CSI was carried in cell content %q", cell.Content)
		}
	})
}

func TestReadLinkPreservesSemicolonsInURL(t *testing.T) {
	const payload = "id=1;https://example.com/a;b?c=1;2"
	var link Link
	ReadLink([]byte("8;"+payload), &link)

	if link.Params != "id=1" || link.URL != "https://example.com/a;b?c=1;2" {
		t.Fatalf("ReadLink() = %#v, want params %q and URL %q", link, "id=1", "https://example.com/a;b?c=1;2")
	}
}

func TestStyledStringLinesReturnsEveryLine(t *testing.T) {
	lines := NewStyledString("one\ntwo\nthree").Lines(ansi.GraphemeWidth)
	if len(lines) != 3 {
		t.Fatalf("Lines() returned %d lines, want 3", len(lines))
	}

	for i, want := range []string{"one", "two", "three"} {
		if got := lines[i].String(); got != want {
			t.Errorf("line %d = %q, want %q", i, got, want)
		}
	}
}

func TestStyledStringCarriesTerminatedAPCBeforeGlyph(t *testing.T) {
	const input = "\x1b_Ga=d,q=2\x1b\\A"
	buf := NewScreenBuffer(32, 1)
	NewStyledString(input).Draw(buf, buf.Bounds())

	cell := buf.CellAt(0, 0)
	if cell == nil {
		t.Fatal("cell is nil")
	}
	if cell.Width != 1 || cell.Content != input {
		t.Fatalf("cell = %#v, want width 1 and content %q", cell, input)
	}
}

func TestStyledStringCarriesTerminatedAPC(t *testing.T) {
	const input = "\x1b_Ga=d,q=2\x1b\\"
	buf := NewScreenBuffer(32, 1)
	NewStyledString(input).Draw(buf, buf.Bounds())

	cell := buf.CellAt(0, 0)
	if cell == nil || cell.Content != input {
		t.Fatalf("cell = %#v, want terminated APC content %q", cell, input)
	}
}

// TestStyledStringCarriesTerminatedStringSequences covers the other prefixes
// that ride in front of a glyph: DCS, SOS and PM, plus the C1 form of the APC
// introducer.
func TestStyledStringCarriesTerminatedStringSequences(t *testing.T) {
	cases := []struct {
		name string
		seq  string
	}{
		{"APC", "\x1b_Ga=d,q=2\x1b\\"},
		{"APC C1 introducer", "\x9fGa=d,q=2\x1b\\"},
		{"DCS", "\x1bPq\x1b\\"},
		{"SOS", "\x1bXhello\x1b\\"},
		{"PM", "\x1b^hello\x1b\\"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := NewScreenBuffer(32, 1)
			NewStyledString(tc.seq+"A").Draw(buf, buf.Bounds())

			cell := buf.CellAt(0, 0)
			if cell == nil {
				t.Fatal("cell is nil")
			}
			if want := tc.seq + "A"; cell.Width != 1 || cell.Content != want {
				t.Fatalf("cell = %#v, want width 1 and content %q", cell, want)
			}
		})
	}
}

// TestStyledStringCarriedSequenceSurvivesTruncationTail pins the carried
// sequence to the cell the tail is written to. Truncation drops the glyph, not
// the sequences that arrived in front of it.
func TestStyledStringCarriedSequenceSurvivesTruncationTail(t *testing.T) {
	const input = "\x1b_Ga=d\x1b\\"
	buf := NewScreenBuffer(5, 1)
	ss := &StyledString{Text: "AB" + input + "CD", Wrap: false, Tail: "..."}
	ss.Draw(buf, buf.Bounds())

	cell := buf.CellAt(2, 0)
	if cell == nil || cell.Content != input+"..." {
		t.Fatalf("tail cell = %#v, want the carried sequence in front of the tail %q", cell, input+"...")
	}
	if got, want := buf.Line(0).String(), "AB"+input+"..."; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

// TestStyledStringLinesKeepsSequenceOnlyContent covers the measurement path: a
// string that is nothing but a carried sequence still yields the line it was
// decomposed into, with a zero-width cell holding the sequence.
func TestStyledStringLinesKeepsSequenceOnlyContent(t *testing.T) {
	const input = "\x1b_Ga=d\x1b\\"

	t.Run("single line", func(t *testing.T) {
		lines := NewStyledString(input).Lines(ansi.GraphemeWidth)
		if len(lines) != 1 {
			t.Fatalf("Lines() returned %d lines, want 1", len(lines))
		}
		cell := lines[0].At(0)
		if cell == nil || cell.Content != input || cell.Width != 0 {
			t.Fatalf("cell = %#v, want a zero-width cell carrying %q", cell, input)
		}
	})

	t.Run("line after a newline", func(t *testing.T) {
		lines := NewStyledString("\n" + input).Lines(ansi.GraphemeWidth)
		if len(lines) != 2 {
			t.Fatalf("Lines() returned %d lines, want 2", len(lines))
		}
		if got := lines[0].String(); got != "" {
			t.Errorf("line 0 = %q, want empty", got)
		}
		if got := lines[1].String(); got != input {
			t.Errorf("line 1 = %q, want %q", got, input)
		}
	})
}

// TestStyledStringFoldsSequenceForOutOfBoundsGlyph covers a glyph that the
// bounds reject: the sequences waiting for it are not consumed by the cell that
// was never written, they land on the last cell that was.
func TestStyledStringFoldsSequenceForOutOfBoundsGlyph(t *testing.T) {
	const input = "\x1b_Ga=d\x1b\\"

	t.Run("glyph past Max.X", func(t *testing.T) {
		buf := NewScreenBuffer(5, 1)
		ss := &StyledString{Text: "AB" + input + "CD", Wrap: false}
		ss.Draw(buf, Rect(0, 0, 2, 1))

		cell := buf.CellAt(1, 0)
		if cell == nil || cell.Content != "B"+input {
			t.Fatalf("last written cell = %#v, want %q", cell, "B"+input)
		}
		if cell := buf.CellAt(2, 0); cell != nil && strings.Contains(cell.Content, input) {
			t.Fatalf("cell past the bounds carries the sequence: %#v", cell)
		}
	})

	t.Run("glyph past the truncation tail", func(t *testing.T) {
		buf := NewScreenBuffer(5, 1)
		ss := &StyledString{Text: "ABC" + input + "DE", Wrap: false, Tail: "..."}
		ss.Draw(buf, buf.Bounds())

		cell := buf.CellAt(2, 0)
		if cell == nil || cell.Content != "..."+input {
			t.Fatalf("last written cell = %#v, want %q", cell, "..."+input)
		}
	})
}

// TestStyledStringCarryWritesNothingOutsideBounds checks that the zero-width
// cell a sequence-only line leaves behind is not drawn outside the rectangle the
// caller asked for.
func TestStyledStringCarryWritesNothingOutsideBounds(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"sequence only", "\x1b_Ga=d\x1b\\"},
		{"sequence before a newline", "\x1b_Ga=d\x1b\\\nA"},
		{"content fills the area", "ABCDEFG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bounds := Rect(1, 0, 4, 1)
			scr := newRecordingScreen(6, 2, bounds)
			s := &StyledString{Text: tc.text, Wrap: false}
			s.Draw(scr, bounds)

			if len(scr.outside) != 0 {
				t.Fatalf("draw wrote outside %v at %v", bounds, scr.outside)
			}
		})
	}
}

// TestStyledStringBelTerminatedSequenceKeepsFollowingText covers a string
// sequence whose payload only ends in BEL. The parser never ends one of those,
// so the text after the BEL would be swallowed along with it.
func TestStyledStringBelTerminatedSequenceKeepsFollowingText(t *testing.T) {
	for _, prefix := range []string{"\x1b_Ga=d", "\x1bPq", "\x1bXa", "\x1b^a", "\x9fGa=d", "\x90q"} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			buf := NewScreenBuffer(16, 1)
			NewStyledString(prefix+"\x07A").Draw(buf, buf.Bounds())

			cell := buf.CellAt(0, 0)
			if cell == nil {
				t.Fatal("cell is nil")
			}
			if cell.Content != "A" {
				t.Fatalf("cell = %#v, want the text after the BEL to print as %q", cell, "A")
			}
		})
	}

	// A sequence that reaches ST keeps its BEL: the parser resolves it, and it
	// is carried whole exactly as before.
	t.Run("ST terminator wins", func(t *testing.T) {
		const input = "\x1b_Ga=d\x07A\x1b\\B"
		buf := NewScreenBuffer(16, 1)
		NewStyledString(input).Draw(buf, buf.Bounds())

		cell := buf.CellAt(0, 0)
		if cell == nil || cell.Content != input {
			t.Fatalf("cell = %#v, want %q", cell, input)
		}
	})
}

// TestStyledStringRejectsExcessiveDCSParams covers the parameter-buffer panic
// for a DCS introducer, the defect the CSI guard already covers: x/ansi parses
// DCS parameters into the same fixed 32-entry buffer and indexes past it.
func TestStyledStringRejectsExcessiveDCSParams(t *testing.T) {
	for _, prefix := range []string{"\x1bP", "\x90"} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			buf := NewScreenBuffer(8, 1)
			NewStyledString(prefix+strings.Repeat(";", 32)+"0qsafe").Draw(buf, buf.Bounds())

			for x, want := range "safe" {
				cell := buf.CellAt(x, 0)
				if cell == nil || cell.Content != string(want) {
					t.Fatalf("cell %d = %#v, want %q", x, cell, string(want))
				}
			}
		})
	}
}

// FuzzStyledStringDrawStaysInBounds covers the invariants the carry accumulator
// has to keep for arbitrary input: a draw writes only inside the area it was
// given, and no malformed sequence reaches the parser.
func FuzzStyledStringDrawStaysInBounds(f *testing.F) {
	seeds := []string{
		"", "A", "AB\x1b_Ga=d\x1b\\CD", "\x1b_Ga=d\x1b\\", "\x1b_Ga=d\x07A",
		"\x1bPq\x1b\\A", "\x1bXa\x1b\\A", "\x1b^a\x1b\\A", "AB\nCD",
		"\x1b_Ga=d\x1b\\\nA", "\n\x1b_Ga=d\x1b\\", "A\x1b_Ga=d\x1b\\\n",
		"\x1bPa1\x1b\\\n", "\x9fGa=d\x1b\\A", "\x90q\x07A", "\x1b]0;t\x1b\\A",
		"\x1bP" + strings.Repeat(";", 32) + "0qsafe",
	}
	for _, seed := range seeds {
		f.Add(seed, false, "...", 5, 2, 1, 0)
	}

	f.Fuzz(func(t *testing.T, text string, wrap bool, tail string, w, h, bx, by int) {
		if w < 1 || w > 12 || h < 1 || h > 4 || bx < 0 || bx >= w || by < 0 || by >= h {
			t.Skip()
		}
		if len(tail) > 4 || len(text) > 128 {
			t.Skip()
		}
		area := Rect(bx, by, w-bx, h-by)
		scr := newRecordingScreen(w, h, area)
		s := &StyledString{Text: text, Wrap: wrap, Tail: tail}
		s.Draw(scr, area)
		s.DrawOver(scr, area)
		if len(scr.outside) != 0 {
			t.Fatalf("draw wrote outside %v at %v (text %q, wrap %v, tail %q)", area, scr.outside, text, wrap, tail)
		}
		_ = NewStyledString(text).Lines(ansi.GraphemeWidth)
	})
}

// recordingScreen records the writes a draw makes so a test can assert that
// nothing lands outside the area it was given.
type recordingScreen struct {
	*Buffer
	method  WidthMethod
	bounds  Rectangle
	outside []Position
}

func newRecordingScreen(width, height int, bounds Rectangle) *recordingScreen {
	return &recordingScreen{Buffer: NewBuffer(width, height), method: ansi.WcWidth, bounds: bounds}
}

func (s *recordingScreen) Bounds() Rectangle { return s.bounds }

func (s *recordingScreen) WidthMethod() WidthMethod { return s.method }

func (s *recordingScreen) SetCell(x, y int, c *Cell) {
	if !Pos(x, y).In(s.bounds) {
		s.outside = append(s.outside, Pos(x, y))
	}
	s.Buffer.SetCell(x, y, c)
}
