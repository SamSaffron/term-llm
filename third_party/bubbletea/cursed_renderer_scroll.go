package tea

import (
	"strings"

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
func (s *cursedRenderer) drawVerticalScroll(newLines []string, shift, top, bottom int) bool {
	if shift == 0 || !s.view.AltScreen || top < 0 || bottom > s.cellbuf.Height() || top >= bottom ||
		!scrollLinesIndependent(s.lastContentLines) || !scrollLinesIndependent(newLines) {
		return false
	}
	if !shiftCellbufRegion(&s.cellbuf, top, bottom, shift) {
		return false
	}
	if !s.scr.HardScroll(s.cellbuf.RenderBuffer, shift, top, bottom-1) {
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

func (s *cursedRenderer) drawChangedRows(newLines []string) bool {
	if !s.view.AltScreen || len(newLines) != len(s.lastContentLines) ||
		len(newLines) > s.cellbuf.Height() || len(s.cellbuf.Touched) < len(newLines) {
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
	if !scrollLinesIndependent(s.lastContentLines) || !scrollLinesIndependent(newLines) {
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
	return true
}

// scrollLinesIndependent reports whether parsing each row from default terminal
// style is equivalent to parsing the complete frame. Plain rows are independent.
// Styled rows must contain only SGR CSI sequences and finish with an explicit
// full reset; OSC links and all cursor/mode controls force the full-frame path.
func scrollLinesIndependent(lines []string) bool {
	for _, line := range lines {
		hasSGR := false
		for i := 0; i < len(line); i++ {
			if line[i] != '\x1b' {
				continue
			}
			hasSGR = true
			if i+2 >= len(line) || line[i+1] != '[' {
				return false
			}
			i += 2
			for i < len(line) && (line[i] < 0x40 || line[i] > 0x7e) {
				i++
			}
			if i >= len(line) || line[i] != 'm' {
				return false
			}
		}
		if hasSGR && !strings.HasSuffix(line, "\x1b[0m") && !strings.HasSuffix(line, "\x1b[m") {
			return false
		}
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
