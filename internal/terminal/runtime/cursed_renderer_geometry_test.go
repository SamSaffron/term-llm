package tea

import (
	"bytes"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestCursedRendererInsertAboveFlushesQueuedView(t *testing.T) {
	output := &boundaryWriter{}
	r := newBoundaryRenderer(output)
	r.render(NewView("stale frame\nstale row"))
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}

	start := len(output.snapshot())
	r.render(NewView("current frame"))
	if err := r.insertAbove("committed block"); err != nil {
		t.Fatal(err)
	}
	joined := bytes.Join(output.snapshot()[start:], nil)
	currentAt := bytes.Index(joined, []byte("current frame"))
	committedAt := bytes.Index(joined, []byte("committed block"))
	if currentAt < 0 || committedAt < 0 || currentAt >= committedAt {
		t.Fatalf("queued frame was not synchronized before scrollback: %q", joined)
	}
	if !r.hasLastView || r.lastView.Content != "current frame" {
		t.Fatalf("lastView = %#v, want current frame", r.lastView)
	}
}

func TestCursedRendererInsertAboveBeforeFirstFlushUsesFrameGeometry(t *testing.T) {
	output := &boundaryWriter{}
	r := newBoundaryRenderer(output)
	r.render(NewView("one-line frame"))
	if err := r.insertAbove("printed from Init"); err != nil {
		t.Fatal(err)
	}

	cursorDown := regexp.MustCompile(`\x1b\[(\d+)B`)
	maxDown := 0
	for _, match := range cursorDown.FindAllStringSubmatch(string(bytes.Join(output.snapshot(), nil)), -1) {
		n, err := strconv.Atoi(match[1])
		if err == nil && n > maxDown {
			maxDown = n
		}
	}
	if maxDown >= 23 {
		t.Fatalf("first Println used terminal height instead of frame height: cursor down %d", maxDown)
	}
}

func TestCursedRendererInsertAboveDoesNotRedrawUnchangedFrame(t *testing.T) {
	output := &boundaryWriter{}
	r := newBoundaryRenderer(output)
	r.render(NewView("stable frame"))
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}
	start := len(output.snapshot())
	if err := r.insertAbove("committed"); err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(output.snapshot()[start:], nil))
	if strings.Count(joined, "stable frame") != 0 {
		t.Fatalf("unchanged synchronization redrew frame: %q", joined)
	}
	if !strings.Contains(joined, "committed") {
		t.Fatalf("missing scrollback insertion: %q", joined)
	}
}

func TestCursedRendererInsertAboveIgnoredInAltScreen(t *testing.T) {
	output := &boundaryWriter{}
	r := newBoundaryRenderer(output)
	view := NewView("stable alt-screen frame")
	view.AltScreen = true
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}

	before := output.snapshot()
	beforeCells := r.cellbuf.Clone()
	if err := r.insertAbove("must not enter the alt screen"); err != nil {
		t.Fatal(err)
	}
	after := output.snapshot()
	if len(after) != len(before) {
		t.Fatalf("alt-screen insertion emitted output: %q", bytes.Join(after[len(before):], nil))
	}
	if !reflect.DeepEqual(r.cellbuf.Lines, beforeCells.Lines) {
		t.Fatal("alt-screen insertion mutated retained cells")
	}
}

func TestCursedRendererResizeRepaintsIdenticalView(t *testing.T) {
	tests := []struct {
		name       string
		altScreen  bool
		width      int
		height     int
		cellHeight int
	}{
		{name: "inline width", width: 32, height: 8, cellHeight: 1},
		{name: "alt screen width", altScreen: true, width: 32, height: 8, cellHeight: 8},
		{name: "alt screen height", altScreen: true, width: 40, height: 10, cellHeight: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := &boundaryWriter{}
			r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, 40, 8)
			view := NewView("identical frame")
			view.AltScreen = tt.altScreen
			r.render(view)
			if err := r.flush(false); err != nil {
				t.Fatal(err)
			}

			start := len(output.snapshot())
			r.resize(tt.width, tt.height)
			r.render(view)
			if err := r.flush(false); err != nil {
				t.Fatal(err)
			}
			joined := bytes.Join(output.snapshot()[start:], nil)
			if !bytes.Contains(joined, []byte("identical frame")) {
				t.Fatalf("identical view was not repainted after resize: %q", joined)
			}
			if r.cellbuf.Width() != tt.width || r.cellbuf.Height() != tt.cellHeight {
				t.Fatalf("retained geometry = %dx%d, want %dx%d", r.cellbuf.Width(), r.cellbuf.Height(), tt.width, tt.cellHeight)
			}
		})
	}
}

func TestCursedRendererInsertAboveCountsExactWidthWraps(t *testing.T) {
	const width = 10
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, 5)
	r.render(NewView("frame"))
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}
	start := len(output.snapshot())
	if err := r.insertAbove(strings.Repeat("x", 2*width)); err != nil {
		t.Fatal(err)
	}
	joined := bytes.Join(output.snapshot()[start:], nil)
	if !bytes.Contains(joined, []byte(ansi.InsertLine(2))) {
		t.Fatalf("two-row exact-width insertion did not reserve two rows: %q", joined)
	}
	if bytes.Contains(joined, []byte(ansi.InsertLine(3))) {
		t.Fatalf("two-row exact-width insertion reserved a spurious third row: %q", joined)
	}
}

func TestBenchmarkFrameUsesRequestedDimensions(t *testing.T) {
	for _, size := range rendererBenchmarkSizes {
		frame := benchmarkFrame(size.width, size.height, 0, 0, true)
		lines := strings.Split(frame, "\n")
		if len(lines) != size.height {
			t.Fatalf("frame height = %d, want %d", len(lines), size.height)
		}
		for row, line := range lines {
			if width := ansi.StringWidth(line); width != size.width {
				t.Fatalf("frame %dx%d row %d width = %d", size.width, size.height, row, width)
			}
		}
	}
}

func TestCursedRendererOnMouseConcurrentFlush(t *testing.T) {
	r := newCursedRenderer(io.Discard, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, 40, 12)
	view := NewView("initial")
	view.OnMouse = func(MouseMsg) Cmd { return nil }
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			view := NewView(strconv.Itoa(i))
			view.OnMouse = func(MouseMsg) Cmd { return nil }
			r.render(view)
			if err := r.flush(false); err != nil {
				t.Errorf("flush: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			_ = r.onMouse(MouseMotionMsg{})
		}
	}()
	wg.Wait()
}
