package tea

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
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
		{name: "OSC hyperlink", lines: []string{"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\"}},
		{name: "cursor control", lines: []string{"\x1b[2Cmove\x1b[0m"}},
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
	if !strings.Contains(joined, "\x1b[3;8r") {
		t.Fatalf("hard scroll did not set expected body region: %q", joined)
	}
}

func bytesJoin(parts [][]byte) []byte {
	var b strings.Builder
	for _, part := range parts {
		b.Write(part)
	}
	return []byte(b.String())
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
			switch i % 6 {
			case 0:
				lines[i] = ""
			case 1:
				lines[i] = "duplicate"
			case 2:
				lines[i] = "界 " + strings.ToValidUTF8(part, "?")
			case 3:
				lines[i] = "\x1b[31m" + strings.ToValidUTF8(part, "?") + "\x1b[0m"
			default:
				lines[i] = strings.ToValidUTF8(part, "?")
			}
		}
		next := append([]string(nil), lines...)
		switch rawMode % 3 {
		case 0:
			for i := 0; i < len(next); i += 5 {
				next[i] = fmt.Sprintf("changed-%d-界", i)
			}
		case 1:
			if len(next) >= 6 {
				copy(next[1:len(next)-1], lines[2:len(lines)-1])
				next[len(next)-2] = "new-scroll-row"
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
			t.Fatalf("incremental and forced-full retained cell/output state differs (terminal=%d content=%d mode=%d)", terminalHeight, contentHeight, rawMode%3)
		}
		if len(incrementalOutput.snapshot()) == 0 || len(fullOutput.snapshot()) == 0 {
			t.Fatal("renderer produced no observable output state")
		}
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
