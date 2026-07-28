package tea

import (
	"strings"
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
)

func splitLinesReuse(dst []string, content string) []string {
	dst = dst[:0]
	for {
		line, rest, found := strings.Cut(content, "\n")
		dst = append(dst, line)
		if !found {
			return dst
		}
		content = rest
	}
}

// detectContentShift finds an exact vertical shift inside unchanged top and
// bottom chrome. Every overlapping row must match exactly. Requiring the full
// overlap deliberately rejects approximate suffix/scrollbar matches: a missed
// optimization is preferable to hard-scrolling the wrong physical rows.
func detectContentShift(oldLines, newLines []string) (shift, top, bottom int) {
	if len(oldLines) != len(newLines) || len(oldLines) < 4 {
		return 0, 0, 0
	}

	top = 0
	for top < len(oldLines) && oldLines[top] == newLines[top] {
		top++
	}
	bottom = len(oldLines)
	for bottom > top && oldLines[bottom-1] == newLines[bottom-1] {
		bottom--
	}

	regionHeight := bottom - top
	if regionHeight < 4 {
		return 0, 0, 0
	}
	maxShift := min(10, regionHeight/2)
	oldRegion := oldLines[top:bottom]
	newRegion := newLines[top:bottom]
	for n := 1; n <= maxShift; n++ {
		if equalStringLines(oldRegion[n:], newRegion[:regionHeight-n]) {
			return n, top, bottom
		}
	}
	for n := 1; n <= maxShift; n++ {
		if equalStringLines(oldRegion[:regionHeight-n], newRegion[n:]) {
			return -n, top, bottom
		}
	}
	return 0, 0, 0
}

func equalStringLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// drawVerticalScroll shifts the retained cell rows and physical terminal, then
// parses only newly exposed rows. UV's HardScroll marks the complete physical
// region touched (the #137/#143 stale-row fix), so repeated or blank rows are
// still re-diffed even when they equal the previous row at the same index.
func (s *cursedRenderer) drawVerticalScroll(newLines []string, newLinesIndependent bool, shift, top, bottom int) bool {
	if shift == 0 || !s.view.AltScreen || top < 0 || bottom > s.cellbuf.Height() || top >= bottom ||
		!s.lastContentIndependent || !newLinesIndependent {
		return false
	}
	if !s.scr.CanHardScroll(s.cellbuf.RenderBuffer, shift, top, bottom-1) {
		return false
	}
	if !shiftCellbufRegion(&s.cellbuf, top, bottom, shift) {
		return false
	}
	if !s.scr.HardScroll(s.cellbuf.RenderBuffer, shift, top, bottom-1) {
		// CanHardScroll proved that HardScroll must accept this geometry. If that
		// contract ever changes, discard both incremental histories rather than
		// continuing from the already-shifted cell buffer.
		s.invalidateIncrementalLocked()
		return false
	}

	// HardScroll's generic #137 fix touches the complete region because arbitrary
	// callers cannot know whether an unchanged-at-index row moved physically. This
	// path has stronger proof: every overlapping source line matched exactly and
	// every line starts from default style state. The shifted physical/cell rows
	// are therefore already exact; only exposed rows need parsing and diffing.
	for i := range s.cellbuf.Touched {
		s.cellbuf.Touched[i] = nil
	}

	width := s.cellbuf.Width()
	if shift > 0 {
		drawStart := bottom - shift
		uv.NewStyledString(strings.Join(newLines[drawStart:bottom], "\n")).DrawOver(
			s.cellbuf,
			uv.Rect(0, drawStart, width, bottom),
		)
	} else {
		drawEnd := top - shift
		uv.NewStyledString(strings.Join(newLines[top:drawEnd], "\n")).DrawOver(
			s.cellbuf,
			uv.Rect(0, top, width, drawEnd),
		)
	}
	return true
}

func (s *cursedRenderer) drawChangedRows(newLines []string, newLinesIndependent bool) bool {
	if !s.view.AltScreen || len(newLines) != len(s.lastContentLines) ||
		len(newLines) > s.cellbuf.Height() || len(s.cellbuf.Touched) < len(newLines) ||
		!s.lastContentIndependent || !newLinesIndependent {
		return false
	}
	var changedRows [16]int
	changedCount := 0
	limit := min(len(changedRows), max(4, len(newLines)/4))
	for row := range newLines {
		if newLines[row] != s.lastContentLines[row] {
			if changedCount >= limit {
				return false
			}
			changedRows[changedCount] = row
			changedCount++
		}
	}
	if changedCount == 0 {
		return false
	}

	width := s.cellbuf.Width()
	for _, row := range changedRows[:changedCount] {
		clearCellLine(s.cellbuf.Lines[row])
		s.cellbuf.Touched[row] = nil
		// Touch the complete old row so an empty or shorter replacement erases
		// physical trailing cells even when DrawOver writes no cell there.
		s.cellbuf.TouchLine(0, row, max(0, width-1))
		uv.NewStyledString(newLines[row]).DrawOver(s.cellbuf, uv.Rect(0, row, width, row+1))
	}
	// The old and new frames are identical at every other row, and each changed
	// row was parsed independently above. A terminal scroll cannot improve this
	// transition, so avoid rebuilding UV's whole-frame line hash map.
	s.scr.SkipScrollOptim()
	return true
}

// scrollLinesIndependent reports whether parsing each row from default terminal
// style is equivalent to parsing the complete frame. Plain rows are independent.
// Styled rows must contain only SGR CSI sequences and finish with an explicit
// full reset; OSC links and all cursor/mode controls force the full-frame path.
func scrollLinesIndependent(lines []string) bool {
	for _, line := range lines {
		if !lineScrollIndependent(line) {
			return false
		}
	}
	return true
}

// contentLinesIndependent reports scrollLinesIndependent(lines) while reusing
// the verdict already proven for the retained frame. Independence is a property
// of a row on its own, so a row whose text is unchanged from the retained frame
// carries that frame's verdict and does not need rescanning. Scanning every row
// of every frame twice per flush was the single largest cost of an incremental
// redraw.
func (s *cursedRenderer) contentLinesIndependent(lines []string) bool {
	if !s.lastContentIndependent {
		// Some retained row failed, so no row inherits a proof from it.
		return scrollLinesIndependent(lines)
	}
	for i, line := range lines {
		if i < len(s.lastContentLines) && line == s.lastContentLines[i] {
			continue
		}
		if !lineScrollIndependent(line) {
			return false
		}
	}
	return true
}

// lineScrollIndependent reports whether one row parses the same in isolation as
// it does inside the complete frame.
func lineScrollIndependent(line string) bool {
	hasSGR := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c >= 0x20 && c < 0x7f {
			// Printable ASCII, by far the common case.
			continue
		}
		if c != '\x1b' {
			// C0 controls and DEL (including tabs, carriage returns, shifts,
			// and cancels) can alter parser/cursor state across row boundaries.
			// Incremental row parsing is safe only for printable text and the
			// self-contained SGR sequences accepted below.
			if c < utf8.RuneSelf {
				return false
			}
			r, size := utf8.DecodeRuneInString(line[i:])
			// The complete-frame grapheme parser can join combining/format
			// code points to state retained from the preceding physical row.
			// Parsing such a row in isolation is not equivalent. C1 controls,
			// separators, unassigned/noncharacter scalars, and other non-graphic
			// values have the same cross-row parser-state risk as C0 controls.
			if unicode.IsControl(r) || unicode.IsMark(r) || unicode.Is(unicode.Cf, r) ||
				(!unicode.IsGraphic(r) && !unicode.Is(unicode.Co, r)) {
				return false
			}
			i += size - 1
			continue
		}
		hasSGR = true
		if i+2 >= len(line) || line[i+1] != '[' {
			return false
		}
		i += 2
		// SGR accepts only parameter bytes before its final 'm'. CSI
		// intermediate bytes (and embedded controls) can make the complete
		// parser carry different state across the following newline.
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

// shiftCellbufRegion rotates lines [top,bottom) by shift positions. Positive
// shift moves content up. Exposed rows are cleared and all touched sentinels
// reset; DrawOver and HardScroll establish the exact rows that must be diffed.
func shiftCellbufRegion(buf *uv.ScreenBuffer, top, bottom, shift int) bool {
	if buf == nil || top < 0 || bottom > buf.Height() || top >= bottom {
		return false
	}
	regionLen := bottom - top
	amount := shift
	if amount < 0 {
		amount = -amount
	}
	if regionLen < 1 || amount < 1 || amount >= regionLen || amount > 10 {
		return false
	}

	region := buf.Lines[top:bottom]
	var saved [10]uv.Line
	if shift > 0 {
		copy(saved[:amount], region[:amount])
		copy(region, region[amount:])
		for i := 0; i < amount; i++ {
			clearCellLine(saved[i])
			region[regionLen-amount+i] = saved[i]
		}
	} else {
		copy(saved[:amount], region[regionLen-amount:])
		copy(region[amount:], region[:regionLen-amount])
		for i := 0; i < amount; i++ {
			clearCellLine(saved[i])
			region[i] = saved[i]
		}
	}
	for i := range buf.Touched {
		buf.Touched[i] = nil
	}
	return true
}

func clearCellLine(line uv.Line) {
	for i := range line {
		line[i] = uv.EmptyCell
	}
}
