package tea

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

func TestDetectContentShiftRequiresExactCompleteOverlap(t *testing.T) {
	tests := []struct {
		name               string
		oldLines, newLines []string
		wantShift          int
		wantTop, wantBot   int
	}{
		{
			name:      "up with fixed chrome",
			oldLines:  []string{"header", "a", "b", "", "b", "c", "footer"},
			newLines:  []string{"header", "b", "", "b", "c", "new", "footer"},
			wantShift: 1,
			wantTop:   1,
			wantBot:   6,
		},
		{
			name:      "down with duplicates",
			oldLines:  []string{"header", "a", "same", "same", "b", "c", "footer"},
			newLines:  []string{"header", "new", "a", "same", "same", "b", "footer"},
			wantShift: -1,
			wantTop:   1,
			wantBot:   6,
		},
		{
			name:     "reject changed suffix",
			oldLines: []string{"header", "a scroll=0", "b scroll=0", "c scroll=0", "d scroll=0", "footer"},
			newLines: []string{"header", "b scroll=1", "c scroll=1", "d scroll=1", "e scroll=1", "footer"},
		},
		{
			name:     "reject partial accidental match",
			oldLines: []string{"header", "a", "b", "c", "d", "e", "footer"},
			newLines: []string{"header", "b", "c", "wrong", "e", "new", "footer"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shift, top, bot := detectContentShift(tt.oldLines, tt.newLines)
			if shift != tt.wantShift || top != tt.wantTop || bot != tt.wantBot {
				t.Fatalf("detectContentShift() = (%d,%d,%d), want (%d,%d,%d)", shift, top, bot, tt.wantShift, tt.wantTop, tt.wantBot)
			}
		})
	}
}

func scrollCorrectnessFrame(lines []string, offset, width, height int) string {
	bodyHeight := height - 4
	rows := make([]string, 0, height)
	rows = append(rows, "\x1b[1mfixed header\x1b[0m", "")
	rows = append(rows, lines[offset:offset+bodyHeight]...)
	rows = append(rows, "", "fixed footer")
	return strings.Join(rows, "\n")
}

func assertRendererCellsEqualContent(t *testing.T, r *cursedRenderer, content string, width, height int) {
	t.Helper()
	want := uv.NewScreenBuffer(width, height)
	uv.NewStyledString(content).Draw(want, want.Bounds())
	if !reflect.DeepEqual(r.cellbuf.Lines, want.Lines) {
		for row := 0; row < height; row++ {
			if !reflect.DeepEqual(r.cellbuf.Lines[row], want.Lines[row]) {
				t.Fatalf("stale cell row %d\ngot:  %#v\nwant: %#v", row, r.cellbuf.Lines[row], want.Lines[row])
			}
		}
		t.Fatal("cell buffers differ")
	}
	rendered := r.scr.RenderedBuffer()
	if rendered == nil || !reflect.DeepEqual(rendered.Lines, want.Lines) {
		t.Fatal("UV retained buffer differs from requested content")
	}
}

func TestScrollLinesIndependent(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{name: "plain", lines: []string{"plain", "界"}, want: true},
		{name: "self-contained SGR", lines: []string{"\x1b[31mred\x1b[0m", "plain"}, want: true},
		{name: "carried SGR", lines: []string{"\x1b[31mred", "inherits red"}},
		{name: "C0 control", lines: []string{"\x01", "plain"}},
		{name: "C1 control", lines: []string{"\u008a", "plain"}},
		{name: "tab control", lines: []string{"column\tshift", "plain"}},
		{name: "leading combining mark", lines: []string{"\u0360", "plain"}},
		{name: "zero width joiner", lines: []string{"joined\u200d", "plain"}},
		{name: "unicode noncharacter", lines: []string{"\uffff", "plain"}},
		{name: "Kitty private-use placeholder", lines: []string{"\U0010eeee", "plain"}, want: true},
		{name: "OSC hyperlink", lines: []string{"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\"}},
		{name: "cursor control", lines: []string{"\x1b[2Cmove\x1b[0m"}},
		{name: "SGR intermediate byte", lines: []string{"\x1b[ m\x1b[m"}},
		{name: "private SGR parameter", lines: []string{"\x1b[?m\x1b[m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrollLinesIndependent(tt.lines); got != tt.want {
				t.Fatalf("scrollLinesIndependent() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestVerticalScrollRejectsCrossLineStyleState(t *testing.T) {
	const width, height = 30, 8
	oldContent := strings.Join([]string{"header", "\x1b[31ma", "b", "c", "d\x1b[0m", "e", "f", "footer"}, "\n")
	newContent := strings.Join([]string{"header", "\x1b[31mb", "c", "d\x1b[0m", "e", "f", "new", "footer"}, "\n")
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)
	view := NewView(oldContent)
	view.AltScreen = true
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}
	r.scr.SetScrollOptim(false)
	start := len(output.snapshot())
	view = NewView(newContent)
	view.AltScreen = true
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}
	assertRendererCellsEqualContent(t, r, newContent, width, height)
	joined := string(bytesJoin(output.snapshot()[start:]))
	if strings.Contains(joined, "\x1b[2;7r") {
		t.Fatalf("unsafe cross-line style shift used explicit fast path: %q", joined)
	}
}

func TestChangedRowsFastPathClearsShortBlankWideAndStyledRows(t *testing.T) {
	const width, height = 24, 6
	frames := []string{
		strings.Join([]string{"header", "a long physical row", "same", "界界", "\x1b[31mred\x1b[0m", "footer"}, "\n"),
		strings.Join([]string{"header", "short", "same", "界x", "\x1b[32mgreen\x1b[0m", "footer"}, "\n"),
		strings.Join([]string{"header", "", "same", "界界", "plain", "footer"}, "\n"),
	}
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)
	for i, content := range frames {
		view := NewView(content)
		view.AltScreen = true
		r.render(view)
		if err := r.flush(false); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		assertRendererCellsEqualContent(t, r, content, width, height)
	}
}

func TestChangedRowsFastPathClipsConsecutiveFramesTallerThanTerminal(t *testing.T) {
	const width, height = 24, 4
	first := strings.Join([]string{"header", "visible", "same", "footer", "offscreen-a", "offscreen-b"}, "\n")
	second := strings.Join([]string{"header", "changed", "same", "footer", "offscreen-c", "offscreen-b"}, "\n")

	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)
	for i, content := range []string{first, second} {
		view := NewView(content)
		view.AltScreen = true
		r.render(view)
		if err := r.flush(false); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		assertRendererCellsEqualContent(t, r, content, width, height)
	}
}

func TestVerticalScrollFastPathPreservesRepeatedBlankAndWideRows(t *testing.T) {
	const width, height = 40, 12
	alphabet := []string{
		"same",
		"",
		"same",
		"界 wide",
		"\x1b[31mstyled duplicate\x1b[0m",
		"same",
		"",
	}
	rng := rand.New(rand.NewSource(1))
	lines := make([]string, 600)
	for i := range lines {
		lines[i] = alphabet[rng.Intn(len(alphabet))]
	}

	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)
	offset := 200
	content := scrollCorrectnessFrame(lines, offset, width, height)
	view := NewView(content)
	view.AltScreen = true
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatalf("initial flush: %v", err)
	}
	assertRendererCellsEqualContent(t, r, content, width, height)

	for frame := 0; frame < 500; frame++ {
		if frame%7 == 0 {
			offset--
		} else {
			offset++
		}
		content = scrollCorrectnessFrame(lines, offset, width, height)
		view = NewView(content)
		view.AltScreen = true
		r.render(view)
		if err := r.flush(false); err != nil {
			t.Fatalf("frame %d offset %d: %v", frame, offset, err)
		}
		assertRendererCellsEqualContent(t, r, content, width, height)
	}
}

func TestVerticalScrollFastPathEmitsScrollRegionBytes(t *testing.T) {
	const width, height = 30, 10
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %02d", i)
	}
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)
	for offset := 0; offset < 2; offset++ {
		view := NewView(scrollCorrectnessFrame(lines, offset, width, height))
		view.AltScreen = true
		r.render(view)
		if err := r.flush(false); err != nil {
			t.Fatal(err)
		}
	}
	joined := string(bytesJoin(output.snapshot()))
	set := "\x1b[3;8r"
	reset := "\x1b[1;10r"
	setAt, resetAt := strings.Index(joined, set), strings.Index(joined, reset)
	if setAt < 0 {
		t.Fatalf("hard scroll did not set expected body region: %q", joined)
	}
	if resetAt < setAt+len(set) {
		t.Fatalf("hard scroll did not reset body region after use: %q", joined)
	}
	terminal := newVTTestTerminal(width, height)
	if err := terminal.apply([]byte(joined)); err != nil {
		t.Fatalf("interpret renderer output: %v", err)
	}
	if err := terminal.assertBalancedMargins(); err != nil {
		t.Fatal(err)
	}
}

func TestVerticalScrollDoesNotMutateCellbufWhenHardScrollPreconditionFails(t *testing.T) {
	const width, height = 30, 8
	oldLines := []string{"header", "a", "b", "c", "d", "e", "f", "footer"}
	newLines := []string{"header", "b", "c", "d", "e", "f", "new", "footer"}
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)
	view := NewView(strings.Join(oldLines, "\n"))
	view.AltScreen = true
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}

	before := r.cellbuf.Clone()
	r.scr.SetFullscreen(false)
	r.view = NewView(strings.Join(newLines, "\n"))
	r.view.AltScreen = true
	shift, top, bottom := detectContentShift(oldLines, newLines)
	if shift == 0 {
		t.Fatal("test setup did not produce an exact shift")
	}
	if r.drawVerticalScroll(newLines, scrollLinesIndependent(newLines), shift, top, bottom) {
		t.Fatal("vertical scroll succeeded without fullscreen HardScroll support")
	}
	if !reflect.DeepEqual(r.cellbuf.Lines, before.Lines) {
		t.Fatal("failed HardScroll precondition mutated retained cell rows")
	}
}

func bytesJoin(parts [][]byte) []byte {
	var b strings.Builder
	for _, part := range parts {
		b.Write(part)
	}
	return []byte(b.String())
}

func FuzzScrollLinesIndependentImpliesEquivalentRowParsing(f *testing.F) {
	f.Add([]byte("plain\x00\x1b[31mred\x1b[0m\x00界"))
	f.Add([]byte("\x1b[1mbold\x1b[m\x00next"))
	f.Add([]byte("\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\"))
	f.Fuzz(func(t *testing.T, seed []byte) {
		if len(seed) > 512 {
			seed = seed[:512]
		}
		rawLines := strings.Split(string(seed), "\x00")
		if len(rawLines) > 8 {
			rawLines = rawLines[:8]
		}
		lines := make([]string, len(rawLines))
		for i, line := range rawLines {
			if len(line) > 64 {
				line = line[:64]
			}
			lines[i] = strings.ToValidUTF8(line, "?")
		}
		if !scrollLinesIndependent(lines) {
			return
		}

		const width = 256
		whole := uv.NewScreenBuffer(width, len(lines))
		uv.NewStyledString(strings.Join(lines, "\n")).Draw(whole, whole.Bounds())
		rows := uv.NewScreenBuffer(width, len(lines))
		for row, line := range lines {
			uv.NewStyledString(line).DrawOver(rows, uv.Rect(0, row, width, row+1))
		}
		if !reflect.DeepEqual(whole.Lines, rows.Lines) {
			t.Fatalf("independent row parsing differs from complete-frame parsing for %#q", lines)
		}
	})
}

func FuzzShiftCellbufRegionMatchesRotation(f *testing.F) {
	f.Add(uint8(8), int8(1), uint8(1), uint8(7))
	f.Add(uint8(8), int8(-2), uint8(0), uint8(8))
	f.Add(uint8(4), int8(12), uint8(0), uint8(4))
	f.Fuzz(func(t *testing.T, rawHeight uint8, rawShift int8, rawTop, rawBottom uint8) {
		height := 1 + int(rawHeight%32)
		top := int(rawTop % uint8(height+2))
		bottom := int(rawBottom % uint8(height+2))
		shift := int(rawShift)
		buf := uv.NewScreenBuffer(4, height)
		for row := 0; row < height; row++ {
			buf.SetCell(0, row, &uv.Cell{Content: fmt.Sprintf("%02d", row), Width: 1})
			buf.TouchLine(0, row, 3)
		}
		before := buf.Clone()
		beforeTouched := make([]*uv.LineData, len(buf.Touched))
		for row, touched := range buf.Touched {
			if touched != nil {
				copy := *touched
				beforeTouched[row] = &copy
			}
		}

		ok := shiftCellbufRegion(&buf, top, bottom, shift)
		amount := shift
		if amount < 0 {
			amount = -amount
		}
		wantOK := top >= 0 && bottom <= height && top < bottom && amount >= 1 && amount < bottom-top && amount <= 10
		if ok != wantOK {
			t.Fatalf("shift result = %v, want %v (height=%d top=%d bottom=%d shift=%d)", ok, wantOK, height, top, bottom, shift)
		}
		if !ok {
			if !reflect.DeepEqual(buf.Lines, before.Lines) || !reflect.DeepEqual(buf.Touched, beforeTouched) {
				t.Fatal("rejected shift mutated the buffer")
			}
			return
		}

		for row := 0; row < height; row++ {
			if row < top || row >= bottom {
				if !reflect.DeepEqual(buf.Lines[row], before.Lines[row]) {
					t.Fatalf("row %d outside shifted region changed", row)
				}
				continue
			}
			source := row + shift
			if source < top || source >= bottom {
				for column := range buf.Lines[row] {
					if buf.Lines[row][column] != uv.EmptyCell {
						t.Fatalf("exposed cell (%d,%d) = %#v, want empty", column, row, buf.Lines[row][column])
					}
				}
			} else if !reflect.DeepEqual(buf.Lines[row], before.Lines[source]) {
				t.Fatalf("row %d does not match source row %d", row, source)
			}
		}
		for row, touched := range buf.Touched {
			if touched != nil {
				t.Fatalf("touched row %d was not reset", row)
			}
		}
	})
}

func FuzzIncrementalRendererMatchesForcedFullRedraw(f *testing.F) {
	f.Add([]byte("plain\x00same\x00\x1b[31mred\x1b[0m\x00界"), uint8(8), uint8(8), uint8(0))
	f.Add([]byte("\x00duplicate\x00duplicate\x00界界"), uint8(5), uint8(14), uint8(1))
	f.Fuzz(func(t *testing.T, seed []byte, rawTerminalHeight, rawContentHeight, rawMode uint8) {
		const width = 32
		terminalHeight := 2 + int(rawTerminalHeight%19)
		contentHeight := 1 + int(rawContentHeight%uint8(terminalHeight+10))
		if len(seed) == 0 {
			seed = []byte("same")
		}
		parts := strings.Split(string(seed), "\x00")
		lines := make([]string, contentHeight)
		for i := range lines {
			part := parts[i%len(parts)]
			if len(part) > 64 {
				part = part[:64]
			}
			// Keep this target focused on byte-stream equivalence for the
			// incremental paths. Malformed CSI can overflow x/ansi before strategy
			// selection, while controls and cross-grapheme code points deliberately
			// force the full-frame path and have focused rejection tests above.
			// Generated wrappers below still exercise valid ANSI sequences.
			part = strings.ReplaceAll(strings.ToValidUTF8(part, "?"), "\x1b", "?")
			part = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) || unicode.IsMark(r) || unicode.Is(unicode.Cf, r) ||
					(!unicode.IsGraphic(r) && !unicode.Is(unicode.Co, r)) {
					return '?'
				}
				return r
			}, part)
			// The VT oracle consumes output rune by rune. Exclude inputs whose
			// grapheme-cluster width differs from the sum of isolated rune widths;
			// terminals cluster these correctly, but the intentionally small oracle
			// would report a false physical-screen divergence.
			isolatedWidth := 0
			for _, r := range part {
				isolatedWidth += ansi.StringWidth(string(r))
			}
			if ansi.StringWidth(part) != isolatedWidth {
				part = strings.Map(func(r rune) rune {
					if r >= unicode.MaxASCII {
						return '?'
					}
					return r
				}, part)
			}
			switch i % 6 {
			case 0:
				lines[i] = ""
			case 1:
				lines[i] = "duplicate"
			case 2:
				lines[i] = "界 " + part
			case 3:
				lines[i] = "\x1b[31m" + part + "\x1b[0m"
			default:
				lines[i] = part
			}
		}
		next := append([]string(nil), lines...)
		switch rawMode % 3 {
		case 0:
			for i := 0; i < len(next); i += 5 {
				next[i] = fmt.Sprintf("changed-%d-界", i)
			}
		case 1:
			amount := 1 + int(rawTerminalHeight%3)
			if len(next) >= amount+5 {
				copy(next[1:len(next)-1], lines[1+amount:len(lines)-1])
				for i := len(next) - 1 - amount; i < len(next)-1; i++ {
					next[i] = fmt.Sprintf("new-scroll-row-%d", i)
				}
			} else {
				next[len(next)-1] = "short-scroll-fallback"
			}
		case 2:
			for i := range next {
				next[i] = fmt.Sprintf("full-%d-%s", i, next[i])
			}
		}

		oldContent := strings.Join(lines, "\n")
		newContent := strings.Join(next, "\n")
		newRenderer := func() (*cursedRenderer, *boundaryWriter) {
			output := &boundaryWriter{}
			r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, terminalHeight)
			view := NewView(oldContent)
			view.AltScreen = true
			r.render(view)
			if err := r.flush(false); err != nil {
				t.Fatalf("initial flush: %v", err)
			}
			return r, output
		}
		incremental, incrementalOutput := newRenderer()
		full, fullOutput := newRenderer()
		full.forceRedraw = true
		full.scr.Erase()
		for _, r := range []*cursedRenderer{incremental, full} {
			view := NewView(newContent)
			view.AltScreen = true
			r.render(view)
			if err := r.flush(false); err != nil {
				t.Fatalf("next flush: %v", err)
			}
		}

		if !reflect.DeepEqual(incremental.cellbuf.Lines, full.cellbuf.Lines) ||
			!reflect.DeepEqual(incremental.cellbuf.RenderBuffer, full.cellbuf.RenderBuffer) ||
			!reflect.DeepEqual(incremental.lastContentLines, full.lastContentLines) {
			t.Fatalf("incremental and forced-full retained cell state differs (terminal=%d content=%d mode=%d)", terminalHeight, contentHeight, rawMode%3)
		}
		// RenderedBuffer is only UV's retained belief; it is useful for checking
		// internal bookkeeping but is not an oracle for the bytes a terminal saw.
		if !reflect.DeepEqual(incremental.scr.RenderedBuffer(), full.scr.RenderedBuffer()) {
			t.Fatalf("incremental and forced-full UV retained state differs (terminal=%d content=%d mode=%d)", terminalHeight, contentHeight, rawMode%3)
		}
		incrementalStream := bytesJoin(incrementalOutput.snapshot())
		fullStream := bytesJoin(fullOutput.snapshot())
		if len(incrementalStream) == 0 || len(fullStream) == 0 {
			t.Fatal("renderer produced no terminal output bytes")
		}
		incrementalTerminal := newVTTestTerminal(width, terminalHeight)
		fullTerminal := newVTTestTerminal(width, terminalHeight)
		if err := incrementalTerminal.apply(incrementalStream); err != nil {
			t.Fatalf("interpret incremental output: %v", err)
		}
		if err := fullTerminal.apply(fullStream); err != nil {
			t.Fatalf("interpret forced-full output: %v", err)
		}
		if err := incrementalTerminal.assertBalancedMargins(); err != nil {
			t.Fatalf("incremental output left terminal state unsafe: %v", err)
		}
		if err := fullTerminal.assertBalancedMargins(); err != nil {
			t.Fatalf("forced-full output left terminal state unsafe: %v", err)
		}
		assertVTTestTerminalsEqual(t, incrementalTerminal, fullTerminal)
	})
}

func FuzzDetectContentShiftExactOverlap(f *testing.F) {
	f.Add(uint8(8), uint8(1), true)
	f.Add(uint8(20), uint8(3), false)
	f.Fuzz(func(t *testing.T, rawHeight, rawShift uint8, up bool) {
		height := 4 + int(rawHeight%40)
		amount := 1 + int(rawShift%10)
		if amount >= height/2 {
			amount = max(1, height/2-1)
		}
		oldLines := make([]string, height)
		for i := range oldLines {
			oldLines[i] = fmt.Sprintf("row-%d", i)
		}
		newLines := make([]string, height)
		if up {
			copy(newLines, oldLines[amount:])
			for i := height - amount; i < height; i++ {
				newLines[i] = fmt.Sprintf("new-%d", i)
			}
		} else {
			copy(newLines[amount:], oldLines[:height-amount])
			for i := 0; i < amount; i++ {
				newLines[i] = fmt.Sprintf("new-%d", i)
			}
		}
		shift, top, bot := detectContentShift(oldLines, newLines)
		if shift == 0 {
			return
		}
		regionHeight := bot - top
		if shift > 0 && !equalStringLines(oldLines[top+shift:bot], newLines[top:top+regionHeight-shift]) {
			t.Fatalf("positive shift %d did not preserve exact overlap", shift)
		}
		if shift < 0 && !equalStringLines(oldLines[top:bot+shift], newLines[top-shift:bot]) {
			t.Fatalf("negative shift %d did not preserve exact overlap", shift)
		}
	})
}
