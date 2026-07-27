//go:build !windows

package tea

import (
	"errors"
	"reflect"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
)

func TestRendererTickerPanicRestoresRawTTYState(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("open pseudoterminal: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	before, err := term.GetState(tty.Fd())
	if err != nil {
		t.Fatalf("read initial TTY state: %v", err)
	}
	output := &boundaryWriter{}
	model := &rendererLifecycleModel{cleanup: testKittyCleanup}
	p := NewProgram(model,
		WithInput(tty),
		WithOutput(output),
		WithWindowSize(80, 24),
		WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
		WithoutSignalHandler(),
		WithFPS(120),
	)
	p.renderer = &panicAfterFlushRenderer{cursedRenderer: newBoundaryRenderer(output)}

	if err := waitLifecycleRun(t, runRendererLifecycleProgram(p), "renderer panic with raw TTY"); !errors.Is(err, ErrProgramPanic) {
		t.Fatalf("Run() error = %v, want renderer panic", err)
	}
	after, err := term.GetState(tty.Fd())
	if err != nil {
		t.Fatalf("read restored TTY state: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("renderer panic stranded the input TTY in raw mode\nbefore: %#v\nafter:  %#v", before, after)
	}
}
