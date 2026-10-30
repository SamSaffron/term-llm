//go:build !windows

package tea

import (
	"io"
	"os"
	"slices"
	"testing"

	"github.com/creack/pty"
)

// windowSizeRecorder records every WindowSizeMsg it sees. After the first one
// it asks the runtime to re-query the terminal size, exactly as the SIGWINCH
// handler does, and quits once it has seen want sizes.
type windowSizeRecorder struct {
	sizes []WindowSizeMsg
	want  int
}

func (m *windowSizeRecorder) Init() Cmd { return nil }

func (m *windowSizeRecorder) Update(msg Msg) (Model, Cmd) {
	size, ok := msg.(WindowSizeMsg)
	if !ok {
		return m, nil
	}
	m.sizes = append(m.sizes, size)
	if len(m.sizes) >= m.want {
		return m, Quit
	}
	return m, func() Msg { return windowSizeMsg{} }
}

func (m *windowSizeRecorder) View() View { return NewView("") }

// openZeroSizePTY returns a pseudoterminal whose kernel window size is 0x0, as
// some containers, serial consoles and detached PTYs report. Output written to
// the slave side is drained so the program never blocks on it.
func openZeroSizePTY(t *testing.T) *os.File {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("open pseudoterminal: %v", err)
	}
	t.Cleanup(func() {
		_ = tty.Close()
		_ = ptmx.Close()
	})
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 0, Cols: 0}); err != nil {
		t.Fatalf("set 0x0 window size: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()
	return tty
}

func TestZeroTerminalSizeFallsBack(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    WindowSizeMsg
	}{
		{"defaults", []string{"TERM=xterm-256color"}, WindowSizeMsg{Width: 80, Height: 24}},
		{"environment", []string{"TERM=xterm-256color", "COLUMNS=132", "LINES=43"}, WindowSizeMsg{Width: 132, Height: 43}},
		{"invalid environment", []string{"TERM=xterm-256color", "COLUMNS=wide", "LINES=-3"}, WindowSizeMsg{Width: 80, Height: 24}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tty := openZeroSizePTY(t)
			model := &windowSizeRecorder{want: 2}
			p := NewProgram(model,
				WithInput(nil),
				WithOutput(tty),
				WithEnvironment(tt.environ),
				WithoutSignalHandler(),
			)
			if err := waitLifecycleRun(t, runRendererLifecycleProgram(p), "zero-size terminal program"); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			// The first size comes from Run, the second from the resize
			// re-query that SIGWINCH triggers; both must fall back.
			if len(model.sizes) != 2 {
				t.Fatalf("got %d window sizes, want 2: %v", len(model.sizes), model.sizes)
			}
			for i, got := range model.sizes {
				if got != tt.want {
					t.Errorf("window size %d = %+v, want %+v", i, got, tt.want)
				}
			}
		})
	}
}

func TestFallbackDimensionsKeepsPositiveSides(t *testing.T) {
	p := NewProgram(nil, WithEnvironment([]string{"COLUMNS=132", "LINES=43"}))
	for _, tt := range []struct{ w, h, wantW, wantH int }{
		{120, 40, 120, 40},
		{0, 50, 132, 50},
		{100, 0, 100, 43},
		{-1, -1, 132, 43},
	} {
		if w, h := p.fallbackDimensions(tt.w, tt.h); w != tt.wantW || h != tt.wantH {
			t.Errorf("fallbackDimensions(%d, %d) = %dx%d, want %dx%d", tt.w, tt.h, w, h, tt.wantW, tt.wantH)
		}
	}
}

func TestImplausibleWindowSizesAreIgnored(t *testing.T) {
	model := &windowSizeRecorder{want: 2}
	p := NewProgram(model,
		WithInput(nil),
		WithOutput(io.Discard),
		WithWindowSize(80, 24),
		WithoutSignalHandler(),
	)
	// Without a TTY the re-query the recorder issues is a no-op, so only the
	// sizes sent here and the initial size reach the model.
	done := runRendererLifecycleProgram(p)
	for _, size := range []WindowSizeMsg{
		{Width: -1, Height: 24},
		{Width: 80, Height: -1},
		{Width: 5000, Height: 1},
		{Width: 1, Height: 5000},
		{Width: 2000, Height: 2000},
		{Width: 120, Height: 40},
	} {
		p.Send(size)
	}
	if err := waitLifecycleRun(t, done, "implausible size program"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// Run delivers the initial size asynchronously, so it may arrive on
	// either side of the sizes sent above.
	if !slices.Contains(model.sizes, WindowSizeMsg{Width: 80, Height: 24}) ||
		!slices.Contains(model.sizes, WindowSizeMsg{Width: 120, Height: 40}) || len(model.sizes) != 2 {
		t.Fatalf("window sizes = %v, want only the initial 80x24 and 120x40", model.sizes)
	}
}

func TestIsPlausibleWindowSize(t *testing.T) {
	for _, tt := range []struct {
		w, h int
		want bool
	}{
		{80, 24, true},
		{0, 0, true},
		{4096, 256, true},
		{1024, 1024, true},
		{-1, 24, false},
		{80, -1, false},
		{4097, 1, false},
		{1, 4097, false},
		{2000, 2000, false},
	} {
		if got := isPlausibleWindowSize(tt.w, tt.h); got != tt.want {
			t.Errorf("isPlausibleWindowSize(%d, %d) = %v, want %v", tt.w, tt.h, got, tt.want)
		}
	}
}
