package uv

import (
	"bytes"
	"image/color"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// StyledString is a string that can be decomposed into a series of styled
// lines and cells. It is used to disassemble a rendered string with ANSI
// escape codes into a series of cells that can be used in a [Buffer].
// A StyledString supports reading [ansi.SGR] and [ansi.Hyperlink] escape
// codes.
type StyledString struct {
	// Text is the original string that was used to create the styled string.
	Text string
	// Wrap determines whether the styled string should wrap to the next line.
	Wrap bool
	// Tail is the string that will be appended to the end of the line when the
	// string is truncated i.e. when [StyledString.Wrap] is false.
	Tail string
}

var _ Drawable = (*StyledString)(nil)

// NewStyledString creates a new [StyledString] for the given method and styled
// string. The method is used to calculate the width of each line.
func NewStyledString(str string) *StyledString {
	ss := new(StyledString)
	ss.Text = str
	return ss
}

// String returns the text of the styled string.
//
// It implements the [fmt.Stringer] interface.
func (s *StyledString) String() string {
	return s.Text
}

// Lines returns the styled string decomposed into a slice of [Line]s.
func (s *StyledString) Lines(m ansi.Method) []Line {
	return printString(nil, m, 0, 0, Rectangle{}, s.Text, false, "")
}

// Draw renders the styled string to the given buffer at the
// specified area.
func (s *StyledString) Draw(buf Screen, area Rectangle) {
	// Clear the area before drawing.
	for y := area.Min.Y; y < area.Max.Y; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			buf.SetCell(x, y, nil)
		}
	}
	str := s.Text
	// We need to normalize newlines "\n" to "\r\n" to emulate a raw terminal
	// output.
	str = strings.ReplaceAll(str, "\r\n", "\n")
	printString(buf, buf.WidthMethod(), area.Min.X, area.Min.Y, area, str, !s.Wrap, s.Tail)
}

// DrawOver renders the styled string to the given buffer at the specified area
// WITHOUT clearing the area first. Cells that haven't changed (as determined
// by SetCell's equality check) will not be marked as touched, enabling
// incremental rendering.
//
// Use DrawOver instead of Draw when the buffer already contains a previous
// frame's content and you want to minimise touched-line tracking for the
// terminal renderer's diff algorithm. The caller is responsible for clearing
// any trailing cells that the new content does not reach.
func (s *StyledString) DrawOver(buf Screen, area Rectangle) {
	str := s.Text
	str = strings.ReplaceAll(str, "\r\n", "\n")
	printString(buf, buf.WidthMethod(), area.Min.X, area.Min.Y, area, str, !s.Wrap, s.Tail)
}

// Height returns the number of lines in the styled string. This is the number
// of lines that the styled string will occupy when rendered to the screen.
func (s *StyledString) Height() int {
	return strings.Count(s.Text, "\n") + 1
}

// UnicodeWidth returns the cells width of the widest line in the styled string
// using the [ansi.GraphemeWidth] method.
func (s *StyledString) UnicodeWidth() int {
	w, _ := s.widthHeight(ansi.GraphemeWidth)
	return w
}

// WcWidth returns the cells width of the widest line in the styled string
// using the [ansi.WcWidth] method.
func (s *StyledString) WcWidth() int {
	w, _ := s.widthHeight(ansi.WcWidth)
	return w
}

func (s *StyledString) widthHeight(m ansi.Method) (w, h int) {
	lines := strings.Split(s.Text, "\n")
	h = len(lines)
	for _, l := range lines {
		w = max(w, m.StringWidth(l))
	}
	return
}

// Bounds returns the minimum area that can contain the whole styled string.
func (s *StyledString) Bounds() Rectangle {
	w, h := s.widthHeight(ansi.GraphemeWidth)
	return Rect(0, 0, w, h)
}

// oversizedParamSequence returns the length of a CSI or DCS sequence with enough
// parameter separators to overflow x/ansi's fixed 32-entry parser buffer, which
// indexes one past it and panics. Both sequence types parse their parameters
// into that buffer, so both need the same guard. A zero result means the input
// is not such a sequence.
func oversizedParamSequence[T []byte | string](str T) int {
	start := 0
	switch {
	case len(str) >= 2 && str[0] == ansi.ESC && (str[1] == '[' || str[1] == 'P'):
		start = 2
	case len(str) >= 1 && (str[0] == ansi.CSI || str[0] == ansi.DCS):
		start = 1
	default:
		return 0
	}

	separators := 0
	for i := start; i < len(str); i++ {
		c := str[i]
		if c == ';' || c == ':' {
			separators++
		}
		if c >= 0x40 && c <= 0x7e {
			if separators >= 32 {
				return i + 1
			}
			return 0
		}
	}
	if separators >= 32 {
		return len(str)
	}
	return 0
}

// malformedSequenceLen returns the length of a control sequence at the start of
// str that the parser cannot resolve, leaving the caller to skip it and continue
// with the text that follows. A zero result means str starts with a sequence the
// parser handles.
func malformedSequenceLen[T []byte | string](str T) int {
	if n := belTerminatedStringSequence(str); n > 0 {
		return n
	}
	return oversizedParamSequence(str)
}

// belTerminatedStringSequence returns the length of a string sequence (APC,
// DCS, SOS, PM) at the start of str whose payload ends in BEL and never reaches
// ST, or zero when str does not begin with one.
//
// The parser only ends these sequences on ST, so it hands back everything up to
// the end of the input as payload and any text after the BEL is swallowed along
// with the malformed sequence. The sequence cannot be carried either: a terminal
// waiting for ST would swallow the cells painted after it. Both problems go away
// by accepting the BEL as the terminator the parser does not: the introducer and
// its payload are dropped, and parsing resumes just past the BEL.
//
// Sequences that do reach ST are untouched. The scan stops at each byte that can
// end the sequence by itself, so an ST, a C1 ST, CAN or SUB leaves the sequence
// to the parser exactly as before.
func belTerminatedStringSequence[T []byte | string](str T) int {
	start := stringSequenceIntroducerLen(str)
	if start == 0 {
		return 0
	}
	bel := -1
	for i := start; i < len(str); i++ {
		switch str[i] {
		case ansi.BEL:
			if bel < 0 {
				bel = i
			}
		case ansi.ESC, ansi.ST, ansi.CAN, ansi.SUB:
			// Any of these settles the sequence: an ST (or the ESC that may
			// begin one) carries it, and CAN or SUB end it as a parser would.
			// The BEL in the payload is only payload in those cases.
			return 0
		}
	}
	if bel < 0 {
		return 0
	}
	return bel + 1
}

// stringSequenceIntroducerLen returns the length of the string sequence
// introducer at the start of str, or zero when str starts with anything else.
func stringSequenceIntroducerLen[T []byte | string](str T) int {
	if len(str) == 0 {
		return 0
	}
	if str[0] == ansi.ESC {
		if len(str) > 1 && (str[1] == '_' || str[1] == 'P' || str[1] == 'X' || str[1] == '^') {
			return 2
		}
		return 0
	}
	if str[0] == ansi.APC || str[0] == ansi.DCS || str[0] == ansi.SOS || str[0] == ansi.PM {
		return 1
	}
	return 0
}

// passThrough reports whether a zero-width sequence is safe to carry in a
// cell's content and replay to the terminal.
func passThrough[T []byte | string](seq T) bool {
	if !ansi.HasApcPrefix(seq) && !ansi.HasDcsPrefix(seq) &&
		!ansi.HasSosPrefix(seq) && !ansi.HasPmPrefix(seq) {
		return false
	}
	return terminated(seq)
}

// terminated reports whether a string-type sequence ended with two-byte ST.
func terminated[T []byte | string](seq T) bool {
	n := len(seq)
	return n >= 2 && seq[n-1] == '\\' && seq[n-2] == ansi.ESC
}

// printString draws a string starting at the given position. If s is nil, it
// will build and return a slice of [Line]s instead (unwrapped, ignoring bounds).
func printString[T []byte | string](
	s Screen,
	m WidthMethod,
	x, y int,
	bounds Rectangle, str T,
	truncate bool, tail string,
) (lines []Line) {
	p := ansi.GetParser()
	defer ansi.PutParser(p)

	var tailc Cell
	if truncate && len(tail) > 0 {
		tailc = *NewCell(m, tail)
	}

	decoder := ansi.DecodeSequenceWc[T]
	if m == ansi.GraphemeWidth {
		decoder = ansi.DecodeSequence[T]
	}

	if s == nil {
		lines = []Line{}
	}

	var cell Cell
	var style Style
	var link Link
	var state byte
	lastX, lastY := -1, -1 // last cell written, for folding in trailing pass-through sequences
	var pending []byte     // pass-through sequences awaiting a cell to ride on
	for len(str) > 0 {
		if n := malformedSequenceLen(str); n > 0 {
			// x/ansi's fixed-size parameter buffer panics when a CSI or DCS
			// contains more parameters than it can retain, and a string
			// sequence whose payload ends in BEL never terminates for the
			// parser at all. Neither can be carried into a cell: treat them as
			// malformed and continue with the text that follows.
			p.Reset()
			state = 0
			str = str[n:]
			continue
		}
		seq, width, n, newState := decoder(str, state, p)
		switch width {
		case 1, 2, 3, 4: // wide cells can go up to 4 cells wide
			cell.Width = width
			cell.Content = string(seq)
			cell.Style = style
			cell.Link = link

			if s == nil {
				// Building lines: unwrapped, no bounds
				if y >= len(lines) {
					lines = append(lines, Line{})
				}
				carryPending(&pending, &cell)
				lines[y] = append(lines[y], cell)
				lastX, lastY = len(lines[y])-1, y
				x += width
			} else {
				// Drawing to screen: handle wrapping, truncation, and bounds
				if !truncate && x+cell.Width > bounds.Max.X && y+1 < bounds.Max.Y {
					// Wrap the string to the width of the window
					x = bounds.Min.X
					y++
				}

				pos := Pos(x, y)
				if pos.In(bounds) {
					if truncate && tailc.Width > 0 && x+cell.Width > bounds.Max.X-tailc.Width {
						// Truncate the string and append the tail if any. The
						// sequences carried in front of the dropped glyph ride
						// on the tail cell, which is written here.
						cell = tailc
						cell.Style = style
						cell.Link = link
						carryPending(&pending, &cell)
						s.SetCell(x, y, &cell)
						lastX, lastY = x, y
						x += tailc.Width
					} else {
						// Print the cell to the screen
						carryPending(&pending, &cell)
						s.SetCell(x, y, &cell)
						lastX, lastY = x, y
						x += width
					}
				}
			}

			// Reset cell for next iteration
			cell = Cell{}
		default:
			// Valid sequences always have a non-zero Cmd.
			// TODO: Handle cursor movement and other sequences
			switch {
			case ansi.HasCsiPrefix(seq) && p.Command() == 'm':
				// SGR - Select Graphic Rendition
				ReadStyle(p.Params(), &style)
			case ansi.HasOscPrefix(seq) && p.Command() == 8:
				// Hyperlinks
				ReadLink(p.Data(), &link)
			case ansi.Equal(seq, T("\n")):
				lines = appendLineSlot(s, lines, y)
				y++
				// Always treat a NL as CR-LF similar to Termios ONLCR.
				fallthrough
			case ansi.Equal(seq, T("\r")):
				if s == nil {
					x = 0
				} else {
					x = bounds.Min.X
				}
			case passThrough(seq):
				pending = append(pending, string(seq)...)
			}
		}

		// Advance the state and data
		state = newState
		str = str[n:]

		if s != nil && y >= bounds.Max.Y {
			// We've reached the bottom of the bounds, stop processing further
			// lines.
			break
		}
	}

	// Pass-through sequences left at the end have no following glyph to carry
	// them, so preserve them on the last cell written, or on a zero-width cell
	// of their own when the string wrote no cell at all.
	lines = foldPendingSequences(s, lines, &cell, lastX, lastY, y, pending)

	// Draw that zero-width cell where the cursor stopped, as long as it is
	// inside the area the caller asked for.
	writeTrailingCell(s, x, y, bounds, &cell)

	return lines
}

// appendLineSlot records empty line slots for leading or consecutive newlines
// while building lines for a nil screen. It is a no-op when drawing to a screen.
func appendLineSlot(s Screen, lines Lines, y int) Lines {
	if s == nil && y >= len(lines) {
		return append(lines, Line{})
	}
	return lines
}

// carryPending prepends the pass-through sequences waiting for a glyph to the
// given cell and clears the accumulator. A carried sequence is zero-width: it
// rides in front of the glyph without changing how the cell measures.
//
// Call it only where the cell is written. A sequence consumed by a cell that is
// never drawn is a sequence the caller never gets back, which is why the
// accumulator is emptied here rather than when the glyph is decoded.
func carryPending(pending *[]byte, cell *Cell) {
	if len(*pending) == 0 {
		return
	}
	*pending = append(*pending, cell.Content...)
	cell.Content = string(*pending)
	*pending = (*pending)[:0]
}

// writeTrailingCell draws the zero-width cell that a sequence-only string left
// behind. A sequence with no cell to ride on is not drawn outside the bounds:
// the caller asked for that rectangle only, and the sequence has no cell inside
// it to go on.
func writeTrailingCell(s Screen, x, y int, bounds Rectangle, cell *Cell) {
	if s == nil || cell.IsZero() || !Pos(x, y).In(bounds) {
		return
	}
	s.SetCell(x, y, cell)
}

// foldPendingSequences preserves pass-through sequences that had no following
// glyph to ride on by attaching them to the last cell written. A string that
// wrote no cell at all leaves them in cell instead: the caller draws that
// zero-width cell where the cursor stopped, and while building lines it is
// appended to the line the cursor stopped on, so a sequence-only string still
// yields the line it was drawn to.
func foldPendingSequences(s Screen, lines Lines, cell *Cell, lastX, lastY, y int, pending []byte) Lines {
	if len(pending) == 0 {
		return lines
	}
	if s == nil {
		if lastY >= 0 && lastY < len(lines) && lastX >= 0 && lastX < len(lines[lastY]) {
			lines[lastY][lastX].Content += string(pending)
			return lines
		}
		lines = appendLineSlot(nil, lines, y)
		lines[y] = append(lines[y], Cell{Content: string(pending)})
		return lines
	}
	if lastX >= 0 {
		if prev := s.CellAt(lastX, lastY); prev != nil {
			folded := *prev
			folded.Content += string(pending)
			s.SetCell(lastX, lastY, &folded)
		}
		return lines
	}
	cell.Content = string(pending)
	return lines
}

// ReadStyle reads a Select Graphic Rendition (SGR) escape sequences from a
// list of parameters into pen.
func ReadStyle(params ansi.Params, pen *Style) {
	if len(params) == 0 {
		*pen = Style{}
		return
	}

	for i := 0; i < len(params); i++ {
		param, hasMore, _ := params.Param(i, 0)
		switch param {
		case 0: // Reset
			*pen = Style{}
		case 1: // Bold
			pen.Attrs |= AttrBold
		case 2: // Dim/Faint
			pen.Attrs |= AttrFaint
		case 3: // Italic
			pen.Attrs |= AttrItalic
		case 4: // Underline
			nextParam, _, ok := params.Param(i+1, 0)
			if hasMore && ok { // Only accept subparameters i.e. separated by ":"
				switch nextParam {
				case 0, 1, 2, 3, 4, 5:
					i++
					switch nextParam {
					case 0: // No Underline
						pen.Underline = UnderlineStyleNone
					case 1: // Single Underline
						pen.Underline = UnderlineStyleSingle
					case 2: // Double Underline
						pen.Underline = UnderlineStyleDouble
					case 3: // Curly Underline
						pen.Underline = UnderlineStyleCurly
					case 4: // Dotted Underline
						pen.Underline = UnderlineStyleDotted
					case 5: // Dashed Underline
						pen.Underline = UnderlineStyleDashed
					}
				}
			} else {
				// Single Underline
				pen.Underline = UnderlineStyleSingle
			}
		case 5: // Slow Blink
			pen.Attrs |= AttrBlink
		case 6: // Rapid Blink
			pen.Attrs |= AttrRapidBlink
		case 7: // Reverse
			pen.Attrs |= AttrReverse
		case 8: // Conceal
			pen.Attrs |= AttrConceal
		case 9: // Crossed-out/Strikethrough
			pen.Attrs |= AttrStrikethrough
		case 22: // Normal Intensity (not bold or faint)
			pen.Attrs &^= (AttrBold | AttrFaint)
		case 23: // Not italic, not Fraktur
			pen.Attrs &^= AttrItalic
		case 24: // Not underlined
			pen.Underline = UnderlineStyleNone
		case 25: // Blink off
			pen.Attrs &^= (AttrBlink | AttrRapidBlink)
		case 27: // Positive (not reverse)
			pen.Attrs &^= AttrReverse
		case 28: // Reveal
			pen.Attrs &^= AttrConceal
		case 29: // Not crossed out
			pen.Attrs &^= AttrStrikethrough
		case 30, 31, 32, 33, 34, 35, 36, 37: // Set foreground
			pen.Fg = ansi.Black + ansi.BasicColor(param-30) //nolint:gosec
		case 38: // Set foreground 256 or truecolor
			var c color.Color
			n := ansi.ReadStyleColor(params[i:], &c)
			if n > 0 {
				pen.Fg = c
				i += n - 1
			}
		case 39: // Default foreground
			pen.Fg = nil
		case 40, 41, 42, 43, 44, 45, 46, 47: // Set background
			pen.Bg = ansi.Black + ansi.BasicColor(param-40) //nolint:gosec
		case 48: // Set background 256 or truecolor
			var c color.Color
			n := ansi.ReadStyleColor(params[i:], &c)
			if n > 0 {
				pen.Bg = c
				i += n - 1
			}
		case 49: // Default Background
			pen.Bg = nil
		case 58: // Set underline color
			var c color.Color
			n := ansi.ReadStyleColor(params[i:], &c)
			if n > 0 {
				pen.UnderlineColor = c
				i += n - 1
			}
		case 59: // Default underline color
			pen.UnderlineColor = nil
		case 90, 91, 92, 93, 94, 95, 96, 97: // Set bright foreground
			pen.Fg = ansi.BrightBlack + ansi.BasicColor(param-90) //nolint:gosec
		case 100, 101, 102, 103, 104, 105, 106, 107: // Set bright background
			pen.Bg = ansi.BrightBlack + ansi.BasicColor(param-100) //nolint:gosec
		}
	}
}

// ReadLink reads a hyperlink escape sequence from a data buffer into link.
func ReadLink(p []byte, link *Link) {
	params := bytes.SplitN(p, []byte{';'}, 3)
	if len(params) != 3 {
		return
	}
	link.Params = string(params[1])
	link.URL = string(params[2])
}
