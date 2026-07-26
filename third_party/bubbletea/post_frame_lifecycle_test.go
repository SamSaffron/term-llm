package tea

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

const testKittyCleanup = "\x1b_Ga=d,i=4242,q=2\x1b\\"

type rendererLifecycleAckMsg struct{}
type rendererLifecycleNoopMsg struct{}
type rendererLifecyclePanicMsg struct{}

type rendererLifecycleModel struct {
	program       *Program
	postStarted   chan struct{}
	allowPost     chan struct{}
	postStartOnce sync.Once
	cleanup       string
	postFrame     bool
	quitOnSuspend bool
}

func (m *rendererLifecycleModel) Init() Cmd { return nil }

func (m *rendererLifecycleModel) Update(msg Msg) (Model, Cmd) {
	switch msg.(type) {
	case rendererLifecyclePanicMsg:
		panic("renderer lifecycle test panic")
	case SuspendMsg:
		if m.quitOnSuspend {
			return m, Quit
		}
	case rendererLifecycleAckMsg, rendererLifecycleNoopMsg, ResumeMsg:
	}
	return m, nil
}

func (m *rendererLifecycleModel) View() View {
	view := NewView("renderer lifecycle frame")
	view.AltScreen = true
	view.MouseMode = MouseModeCellMotion
	view.TerminalCleanup = m.cleanup
	if m.postFrame {
		view.PostFrame = testPostFrame
		view.PostFrameMsg = func(error) Msg {
			m.postStartOnce.Do(func() { close(m.postStarted) })
			<-m.allowPost
			return rendererLifecycleAckMsg{}
		}
	}
	return view
}

func newRendererLifecycleProgram(t *testing.T, model *rendererLifecycleModel, output *boundaryWriter, opts ...ProgramOption) *Program {
	t.Helper()
	programOpts := []ProgramOption{
		WithInput(bytes.NewReader(nil)),
		WithOutput(output),
		WithWindowSize(80, 24),
		WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
		WithoutSignalHandler(),
		WithFPS(120),
	}
	programOpts = append(programOpts, opts...)
	p := NewProgram(model, programOpts...)
	model.program = p
	return p
}

func runRendererLifecycleProgram(p *Program) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()
	return done
}

func waitLifecycle(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitLifecycleRun(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func TestKillDoesNotBlockBeforeRendererLoopStarts(t *testing.T) {
	output := &boundaryWriter{}
	model := &rendererLifecycleModel{cleanup: testKittyCleanup}
	p := NewProgram(model, WithInput(bytes.NewReader(nil)), WithOutput(output), WithWindowSize(80, 24))
	p.renderer = newBoundaryRenderer(output)
	view := NewView("queued before renderer start")
	view.AltScreen = true
	view.TerminalCleanup = testKittyCleanup
	p.renderer.render(view)

	killed := make(chan struct{})
	go func() {
		p.Kill()
		close(killed)
	}()
	waitLifecycle(t, killed, "Kill before renderer loop start")
	if got := bytes.Count(bytes.Join(output.snapshot(), nil), []byte(testKittyCleanup)); got != 1 {
		t.Fatalf("cleanup count = %d, want 1", got)
	}
}

func TestPostFrameResultDoesNotDeadlockExecProcessWithUnacknowledgedPayload(t *testing.T) {
	output := &boundaryWriter{}
	model := &rendererLifecycleModel{
		postStarted: make(chan struct{}),
		allowPost:   make(chan struct{}),
		postFrame:   true,
	}
	p := newRendererLifecycleProgram(t, model, output)
	runDone := runRendererLifecycleProgram(p)
	waitLifecycle(t, model.postStarted, "post-frame result factory")

	execAccepted := make(chan struct{})
	execCompleted := make(chan struct{})
	go func() {
		p.Send(ExecProcess(exec.Command("sh", "-c", "true"), func(err error) Msg {
			close(execCompleted)
			return QuitMsg{}
		})())
		close(execAccepted)
	}()
	// Send returns only after the event loop has accepted execMsg. The event loop
	// cannot receive the post-frame result while it is synchronously releasing
	// the terminal for ExecProcess.
	waitLifecycle(t, execAccepted, "ExecProcess handoff")
	close(model.allowPost)

	waitLifecycle(t, execCompleted, "ExecProcess completion")
	if err := waitLifecycleRun(t, runDone, "program exit after ExecProcess"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestPostFrameResultDoesNotDeadlockSuspendWithUnacknowledgedPayload(t *testing.T) {
	output := &boundaryWriter{}
	model := &rendererLifecycleModel{
		postStarted:   make(chan struct{}),
		allowPost:     make(chan struct{}),
		postFrame:     true,
		quitOnSuspend: true,
	}
	p := newRendererLifecycleProgram(t, model, output)
	suspended := make(chan struct{})
	p.suspendFunc = func() { close(suspended) }
	runDone := runRendererLifecycleProgram(p)
	waitLifecycle(t, model.postStarted, "post-frame result factory")

	suspendAccepted := make(chan struct{})
	go func() {
		p.Send(SuspendMsg{})
		close(suspendAccepted)
	}()
	waitLifecycle(t, suspendAccepted, "SuspendMsg handoff")
	close(model.allowPost)

	waitLifecycle(t, suspended, "suspend lifecycle")
	if err := waitLifecycleRun(t, runDone, "program exit after SuspendMsg"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

type panicAfterFlushRenderer struct {
	*cursedRenderer
	panicked sync.Once
}

func (r *panicAfterFlushRenderer) flush(closing bool) error {
	err := r.cursedRenderer.flush(closing)
	if !closing {
		r.panicked.Do(func() { panic("renderer ticker panic") })
	}
	return err
}

func TestRendererTickerPanicRestoresTerminalModesAndDoesNotDeadlock(t *testing.T) {
	output := &boundaryWriter{}
	model := &rendererLifecycleModel{cleanup: testKittyCleanup}
	p := newRendererLifecycleProgram(t, model, output)
	p.renderer = &panicAfterFlushRenderer{cursedRenderer: newBoundaryRenderer(output)}

	runDone := runRendererLifecycleProgram(p)
	if err := waitLifecycleRun(t, runDone, "renderer panic shutdown"); !errors.Is(err, ErrProgramPanic) {
		t.Fatalf("Run() error = %v, want renderer panic", err)
	}

	joined := bytes.Join(output.snapshot(), nil)
	for name, seq := range map[string]string{
		"cleanup":         testKittyCleanup,
		"alt-screen exit": ansi.ResetModeAltScreenSaveCursor,
		"mouse reset":     ansi.ResetModeMouseButtonEvent,
		"paste reset":     ansi.ResetModeBracketedPaste,
	} {
		if !bytes.Contains(joined, []byte(seq)) {
			t.Fatalf("renderer panic did not restore %s; output=%q", name, joined)
		}
	}
}

func TestProgramShortWriteRecoversRendererAndShutsDown(t *testing.T) {
	output := &boundaryWriter{failContains: []byte(ansi.RequestModeSynchronizedOutput), failAfter: 1}
	model := &rendererLifecycleModel{cleanup: testKittyCleanup}
	p := newRendererLifecycleProgram(t, model, output,
		WithEnvironment([]string{"TERM=xterm-kitty", "TERM_PROGRAM=kitty"}),
	)
	runDone := runRendererLifecycleProgram(p)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		output.mu.Lock()
		failed := output.failedOnce
		output.mu.Unlock()
		if failed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	output.mu.Lock()
	failed := output.failedOnce
	output.mu.Unlock()
	if !failed {
		p.Kill()
		t.Fatal("program-level short write was not exercised")
	}

	p.Quit()
	if err := waitLifecycleRun(t, runDone, "short-write recovery shutdown"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	renderer := p.renderer.(*cursedRenderer)
	if renderer.forceRedraw {
		t.Fatal("short-write recovery left renderer invalid")
	}
	joined := bytes.Join(output.snapshot(), nil)
	if !bytes.Contains(joined, []byte(testKittyCleanup)) || !bytes.Contains(joined, []byte(ansi.ResetModeAltScreenSaveCursor)) {
		t.Fatalf("short-write shutdown did not restore terminal: %q", joined)
	}
}

func TestTerminalCleanupPrecedesAltScreenExit(t *testing.T) {
	output := &boundaryWriter{}
	r := newBoundaryRenderer(output)
	view := NewView("image frame")
	view.AltScreen = true
	view.TerminalCleanup = testKittyCleanup
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatalf("flush image frame: %v", err)
	}
	if err := r.close(); err != nil {
		t.Fatalf("close renderer: %v", err)
	}

	joined := bytes.Join(output.snapshot(), nil)
	cleanupAt := bytes.Index(joined, []byte(testKittyCleanup))
	altExitAt := bytes.Index(joined, []byte(ansi.ResetModeAltScreenSaveCursor))
	if cleanupAt < 0 || altExitAt < 0 || cleanupAt >= altExitAt {
		t.Fatalf("cleanup/alt-exit order cleanup=%d alt-exit=%d output=%q", cleanupAt, altExitAt, joined)
	}
	if got := bytes.Count(joined, []byte(testKittyCleanup)); got != 1 {
		t.Fatalf("cleanup count = %d, want 1; output=%q", got, joined)
	}
}

func TestTerminalCleanupRunsForEveryAbnormalTermination(t *testing.T) {
	tests := []struct {
		name      string
		terminate func(*Program)
		wantErr   error
	}{
		{
			name:      "interrupt message",
			terminate: func(p *Program) { p.Send(InterruptMsg{}) },
			wantErr:   ErrInterrupted,
		},
		{
			name:      "program kill",
			terminate: func(p *Program) { p.Kill() },
			wantErr:   ErrProgramKilled,
		},
		{
			name:      "context cancellation",
			terminate: func(p *Program) { p.cancel() },
			wantErr:   ErrProgramKilled,
		},
		{
			name:      "panic",
			terminate: func(p *Program) { p.Send(rendererLifecyclePanicMsg{}) },
			wantErr:   ErrProgramPanic,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := &boundaryWriter{}
			model := &rendererLifecycleModel{cleanup: testKittyCleanup}
			p := newRendererLifecycleProgram(t, model, output)
			runDone := runRendererLifecycleProgram(p)

			// A no-op round trip proves Run has queued the initial View before the
			// abnormal termination is initiated.
			p.Send(rendererLifecycleNoopMsg{})
			tt.terminate(p)
			err := waitLifecycleRun(t, runDone, "abnormal termination")
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want errors.Is(%v)", err, tt.wantErr)
			}

			joined := bytes.Join(output.snapshot(), nil)
			if got := bytes.Count(joined, []byte(testKittyCleanup)); got != 1 {
				t.Fatalf("Kitty cleanup count = %d, want 1; output=%q", got, joined)
			}
		})
	}
}

func TestTerminalCleanupWithExternalContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &boundaryWriter{}
	model := &rendererLifecycleModel{cleanup: testKittyCleanup}
	p := newRendererLifecycleProgram(t, model, output, WithContext(ctx))
	runDone := runRendererLifecycleProgram(p)
	p.Send(rendererLifecycleNoopMsg{})
	cancel()
	if err := waitLifecycleRun(t, runDone, "external context cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	joined := bytes.Join(output.snapshot(), nil)
	if got := bytes.Count(joined, []byte(testKittyCleanup)); got != 1 {
		t.Fatalf("Kitty cleanup count = %d, want 1; output=%q", got, joined)
	}
}
