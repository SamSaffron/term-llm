package chat

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/termimage"
	"github.com/samsaffron/term-llm/internal/testutil"
	"github.com/samsaffron/term-llm/internal/ui"
)

const (
	probeRawMarker        = "<raw-before-image-frame>"
	probeScrollbackMarker = "<scrollback-before-image-frame>"
	probeImageFrame       = "<image-frame>"
	probeScrollFrame      = "<scroll-frame>"
	probeBottomFrame      = "<bottom-frame>"
	probeResizeFrame      = "<resize-frame>"
	probeResizeSettled    = "<resize-settled-frame>"
	probeQuitFrame        = "<quit-frame>"
	probeShellInitial     = "<shell-initial-frame>"
	probeShellHandoff     = "<shell-handoff-frame>"
	probeShellChild       = "<shell-child-output>"
	probeShellRestored    = "<shell-restored-frame>"
	probeRetryFrame       = "<post-frame-retry-frame>"
)

type (
	probeActivateImageMsg struct{}
	probeStableFrameMsg   struct{}
	probeScrollTopMsg     struct{}
	probeScrollBottomMsg  struct{}
	probeResizeSettledMsg struct{}
	probeShellStartMsg    struct{}
	probeShellExitedMsg   struct{ err error }
	probeRetryImagesMsg   struct{}
	probeQuitMsg          struct{}
	probePanicMsg         struct{}
)

type postFramePipelineProbe struct {
	model   *Model
	path    string
	frame   string
	script  tea.Cmd
	onRetry func()
}

func (p *postFramePipelineProbe) Init() tea.Cmd { return p.script }

func (p *postFramePipelineProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case probeActivateImageMsg:
		p.model.tracker = ui.NewToolTracker()
		p.model.tracker.AddPreRenderedTextSegment(strings.Repeat("scroll filler\n", 40))
		p.model.tracker.AddImageSegment(p.path)
		p.model.streaming = true
		p.model.scrollToBottom = true
		p.model.bumpContentVersion()
		p.frame = probeImageFrame
		return p, nil
	case probeStableFrameMsg:
		// Deliberately leave the complete View unchanged. A fresh PostFrame must
		// still be consumed at Bubble Tea's successful no-op frame boundary.
		return p, nil
	case probeScrollTopMsg:
		p.model.viewport.SetYOffset(0)
		p.model.viewCache.lastSetContentAt = time.Time{}
		p.frame = probeScrollFrame
		return p, nil
	case probeScrollBottomMsg:
		p.model.viewport.GotoBottom()
		p.model.viewCache.lastSetContentAt = time.Time{}
		p.frame = probeBottomFrame
		return p, nil
	case tea.WindowSizeMsg:
		p.frame = probeResizeFrame
		updated, cmd := p.model.Update(msg)
		p.model = updated.(*Model)
		return p, cmd
	case probeResizeSettledMsg:
		p.model.viewCache.lastSetContentAt = time.Time{}
		p.frame = probeResizeSettled
		return p, nil
	case probeShellStartMsg:
		p.model.setShellTerminalHandoff(true)
		p.frame = probeShellHandoff
		cmd := exec.Command("sh", "-c", "printf '%s' '"+probeShellChild+"'") //nolint:gosec
		return p, tea.ExecProcess(cmd, func(err error) tea.Msg { return probeShellExitedMsg{err: err} })
	case probeShellExitedMsg:
		p.model.setShellTerminalHandoff(false)
		p.frame = probeShellRestored
		return p, delayedProbeMsg(60*time.Millisecond, probeQuitMsg{})
	case probeRetryImagesMsg:
		if p.onRetry != nil {
			p.onRetry()
		}
		p.model.resetImageUploadState()
		p.model.invalidateImageViewportContent()
		p.frame = probeRetryFrame
		return p, delayedProbeMsg(80*time.Millisecond, probeQuitMsg{})
	case probeQuitMsg:
		p.frame = probeQuitFrame
		return p, p.model.quitCmd()
	case probePanicMsg:
		panic("post-frame Kitty cleanup panic probe")
	default:
		updated, cmd := p.model.Update(msg)
		p.model = updated.(*Model)
		return p, cmd
	}
}

func (p *postFramePipelineProbe) View() tea.View {
	view := p.model.View()
	view.WindowTitle = p.frame
	return view
}

func delayedProbeMsg(delay time.Duration, msg tea.Msg) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(delay)
		return msg
	}
}

func runPostFramePipelineProbe(t *testing.T, probe *postFramePipelineProbe, output interface{ Write([]byte) (int, error) }, opts ...tea.ProgramOption) error {
	t.Helper()
	programOpts := []tea.ProgramOption{
		tea.WithInput(bytes.NewReader(nil)),
		tea.WithOutput(output),
		tea.WithWindowSize(60, 18),
		tea.WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
		tea.WithoutSignalHandler(),
		tea.WithFPS(120),
	}
	programOpts = append(programOpts, opts...)
	program := tea.NewProgram(probe, programOpts...)
	probe.model.SetProgram(program)
	_, err := program.Run()
	return err
}

func testWriteContaining(writes [][]byte, marker string, start int) int {
	for i := max(0, start); i < len(writes); i++ {
		if bytes.Contains(writes[i], []byte(marker)) {
			return i
		}
	}
	return -1
}

func testWritesContaining(writes [][]byte, marker string, start, end int) []int {
	if end < 0 || end > len(writes) {
		end = len(writes)
	}
	var matches []int
	for i := max(0, start); i < end; i++ {
		if bytes.Contains(writes[i], []byte(marker)) {
			matches = append(matches, i)
		}
	}
	return matches
}

func newImagePipelineProbe(t *testing.T, frame string) *postFramePipelineProbe {
	t.Helper()
	model := newTestChatModel(true)
	model.width = 60
	model.height = 18
	model.syncAltScreenViewportHeight(model.buildFooterLayout().height)
	return &postFramePipelineProbe{model: model, path: writeChatTestPNG(t), frame: frame}
}

func TestRealBubbleTeaPostFramePipelineLifecycleOrdering(t *testing.T) {
	requireFullTestSuite(t)
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	probe := newImagePipelineProbe(t, "<blank-frame>")
	probe.script = tea.Sequence(
		tea.Raw(probeRawMarker),
		tea.Println(probeScrollbackMarker),
		delayedProbeMsg(30*time.Millisecond, probeActivateImageMsg{}),
		delayedProbeMsg(50*time.Millisecond, probeStableFrameMsg{}),
		delayedProbeMsg(50*time.Millisecond, probeScrollTopMsg{}),
		delayedProbeMsg(50*time.Millisecond, probeScrollBottomMsg{}),
		delayedProbeMsg(50*time.Millisecond, tea.WindowSizeMsg{Width: 48, Height: 16}),
		delayedProbeMsg(140*time.Millisecond, probeResizeSettledMsg{}),
		delayedProbeMsg(60*time.Millisecond, probeQuitMsg{}),
	)
	capture := testutil.NewByteCapture()
	if err := runPostFramePipelineProbe(t, probe, capture); err != nil {
		t.Fatalf("Bubble Tea Run() error = %v", err)
	}

	writes := capture.Writes()
	rawWrite := testWriteContaining(writes, probeRawMarker, 0)
	scrollbackWrite := testWriteContaining(writes, probeScrollbackMarker, 0)
	imageFrameWrite := testWriteContaining(writes, probeImageFrame, 0)
	scrollFrameWrite := testWriteContaining(writes, probeScrollFrame, imageFrameWrite+1)
	if rawWrite < 0 || imageFrameWrite < 0 || scrollFrameWrite < 0 {
		t.Fatalf("missing lifecycle markers raw=%d image-frame=%d scroll-frame=%d in %q", rawWrite, imageFrameWrite, scrollFrameWrite, writes)
	}
	if scrollbackWrite >= 0 {
		t.Fatalf("Println emitted unmanaged scrollback in alt-screen mode at write %d", scrollbackWrite)
	}
	if rawWrite >= imageFrameWrite {
		t.Fatalf("raw ordering raw=%d image-frame=%d, want raw output before image frame", rawWrite, imageFrameWrite)
	}
	placements := testWritesContaining(writes, "a=p", imageFrameWrite+1, scrollFrameWrite)
	if len(placements) != 1 {
		t.Fatalf("stable unchanged frame emitted %d placements between image and scroll frames, want exactly the initial acknowledged placement; writes=%q", len(placements), writes[imageFrameWrite:scrollFrameWrite+1])
	}
	if uploads := testWritesContaining(writes, "a=t", placements[0]+1, scrollFrameWrite); len(uploads) != 0 {
		t.Fatalf("stable unchanged frame retransmitted %d acknowledged uploads: writes=%q", len(uploads), writes[imageFrameWrite:scrollFrameWrite+1])
	}

	scrollDeleteWrite := testWriteContaining(writes, "a=d,d=i", scrollFrameWrite+1)
	bottomFrameWrite := testWriteContaining(writes, probeBottomFrame, scrollFrameWrite+1)
	if scrollDeleteWrite < 0 || bottomFrameWrite < 0 || scrollDeleteWrite >= bottomFrameWrite {
		t.Fatalf("scroll ordering scroll-frame=%d delete=%d bottom-frame=%d", scrollFrameWrite, scrollDeleteWrite, bottomFrameWrite)
	}

	resizeFrameWrite := testWriteContaining(writes, probeResizeFrame, bottomFrameWrite+1)
	resizeCleanupWrite := testWriteContaining(writes, "a=d,i=", resizeFrameWrite+1)
	resizeUploadWrite := testWriteContaining(writes, "a=t", resizeFrameWrite+1)
	resizePlacementWrite := testWriteContaining(writes, "a=p", resizeFrameWrite+1)
	resizeSettledWrite := testWriteContaining(writes, probeResizeSettled, resizeFrameWrite+1)
	if resizeFrameWrite < 0 || resizeCleanupWrite < 0 || resizeUploadWrite < 0 || resizePlacementWrite < 0 || resizeSettledWrite < 0 {
		t.Fatalf("missing resize markers frame=%d cleanup=%d upload=%d placement=%d settled=%d writes=%q", resizeFrameWrite, resizeCleanupWrite, resizeUploadWrite, resizePlacementWrite, resizeSettledWrite, writes)
	}
	if resizeCleanupWrite <= resizeFrameWrite || resizeUploadWrite != resizeCleanupWrite || resizePlacementWrite != resizeCleanupWrite || resizeSettledWrite <= resizePlacementWrite {
		t.Fatalf("resize side-band was not one acknowledged cleanup/upload/placement transition: frame=%d cleanup=%d upload=%d placement=%d settled=%d", resizeFrameWrite, resizeCleanupWrite, resizeUploadWrite, resizePlacementWrite, resizeSettledWrite)
	}

	quitFrameWrite := testWriteContaining(writes, probeQuitFrame, resizeSettledWrite+1)
	quitCleanupWrite := testWriteContaining(writes, "a=d,i=", quitFrameWrite+1)
	altExitWrite := testWriteContaining(writes, ansi.ResetModeAltScreenSaveCursor, quitCleanupWrite)
	if quitFrameWrite < 0 || quitCleanupWrite < 0 || altExitWrite < 0 || quitFrameWrite >= quitCleanupWrite || quitCleanupWrite > altExitWrite {
		t.Fatalf("quit ordering frame=%d cleanup=%d alt-exit=%d", quitFrameWrite, quitCleanupWrite, altExitWrite)
	}
	if quitCleanupWrite == altExitWrite {
		cleanupAt := bytes.Index(writes[quitCleanupWrite], []byte("a=d,i="))
		altExitAt := bytes.Index(writes[altExitWrite], []byte(ansi.ResetModeAltScreenSaveCursor))
		if cleanupAt < 0 || altExitAt < 0 || cleanupAt >= altExitAt {
			t.Fatalf("same-write quit cleanup order cleanup=%d alt-exit=%d bytes=%q", cleanupAt, altExitAt, writes[quitCleanupWrite])
		}
	}
	if placementAfterQuit := testWriteContaining(writes, "a=p", quitFrameWrite+1); placementAfterQuit >= 0 {
		t.Fatalf("quitting View armed a placement at write %d after quit frame %d", placementAfterQuit, quitFrameWrite)
	}
}

func TestRealBubbleTeaPostFramePipelineExecProcessHandoff(t *testing.T) {
	requireFullTestSuite(t)
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	probe := newImagePipelineProbe(t, probeShellInitial)
	probe.model.tracker = ui.NewToolTracker()
	probe.model.tracker.AddImageSegment(probe.path)
	probe.model.streaming = true
	probe.model.bumpContentVersion()
	probe.script = delayedProbeMsg(60*time.Millisecond, probeShellStartMsg{})
	capture := testutil.NewByteCapture()
	if err := runPostFramePipelineProbe(t, probe, capture); err != nil {
		t.Fatalf("Bubble Tea Run() error = %v", err)
	}

	writes := capture.Writes()
	initialPlacement := testWriteContaining(writes, "a=p", 0)
	childWrite := testWriteContaining(writes, probeShellChild, initialPlacement+1)
	cleanupBeforeChild := testWriteContaining(writes, "a=d,i=", initialPlacement+1)
	restoredFrame := testWriteContaining(writes, probeShellRestored, childWrite+1)
	restoredPlacement := testWriteContaining(writes, "a=p", restoredFrame+1)
	if initialPlacement < 0 || cleanupBeforeChild < 0 || childWrite < 0 || restoredFrame < 0 || restoredPlacement < 0 {
		t.Fatalf("missing shell markers initial-placement=%d cleanup=%d child=%d restored-frame=%d restored-placement=%d in %q", initialPlacement, cleanupBeforeChild, childWrite, restoredFrame, restoredPlacement, writes)
	}
	if cleanupBeforeChild >= childWrite {
		t.Fatalf("Kitty cleanup did not precede child output: cleanup=%d child=%d", cleanupBeforeChild, childWrite)
	}
	if leaked := testWriteContaining(writes, "a=p", childWrite+1); leaked >= 0 && leaked < restoredFrame {
		t.Fatalf("image placement leaked while child owned terminal at write %d (child=%d restored=%d)", leaked, childWrite, restoredFrame)
	}
}

func TestRealBubbleTeaKittyCleanupOnAbnormalTermination(t *testing.T) {
	requireFullTestSuite(t)
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")

	tests := []struct {
		name    string
		trigger func(*tea.Program, context.CancelFunc)
		wantErr error
	}{
		{name: "interrupt", trigger: func(p *tea.Program, _ context.CancelFunc) { p.Send(tea.InterruptMsg{}) }, wantErr: tea.ErrInterrupted},
		{name: "kill", trigger: func(p *tea.Program, _ context.CancelFunc) { p.Kill() }, wantErr: tea.ErrProgramKilled},
		{name: "context cancellation", trigger: func(_ *tea.Program, cancel context.CancelFunc) { cancel() }, wantErr: context.Canceled},
		{name: "panic", trigger: func(p *tea.Program, _ context.CancelFunc) { p.Send(probePanicMsg{}) }, wantErr: tea.ErrProgramPanic},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			termimage.ClearCache()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := newImagePipelineProbe(t, probeImageFrame)
			probe.model.tracker = ui.NewToolTracker()
			probe.model.tracker.AddImageSegment(probe.path)
			probe.model.streaming = true
			probe.model.bumpContentVersion()
			capture := testutil.NewByteCapture()
			program := tea.NewProgram(probe,
				tea.WithContext(ctx),
				tea.WithInput(bytes.NewReader(nil)),
				tea.WithOutput(capture),
				tea.WithWindowSize(60, 18),
				tea.WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
				tea.WithoutSignalHandler(),
				tea.WithFPS(120),
			)
			probe.model.SetProgram(program)
			runDone := make(chan error, 1)
			go func() {
				_, err := program.Run()
				runDone <- err
			}()

			deadline := time.Now().Add(2 * time.Second)
			for testWriteContaining(capture.Writes(), "a=p", 0) < 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if testWriteContaining(capture.Writes(), "a=p", 0) < 0 {
				program.Kill()
				t.Fatal("initial Kitty placement did not reach the terminal")
			}
			tt.trigger(program, cancel)

			var err error
			select {
			case err = <-runDone:
			case <-time.After(2 * time.Second):
				program.Kill()
				t.Fatal("program did not terminate")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want errors.Is(%v)", err, tt.wantErr)
			}

			writes := capture.Writes()
			placement := testWriteContaining(writes, "a=p", 0)
			cleanup := testWriteContaining(writes, "a=d,i=", placement+1)
			altExit := testWriteContaining(writes, ansi.ResetModeAltScreenSaveCursor, cleanup)
			if placement < 0 || cleanup < 0 || altExit < 0 || cleanup > altExit {
				t.Fatalf("abnormal Kitty cleanup order placement=%d cleanup=%d alt-exit=%d writes=%q", placement, cleanup, altExit, writes)
			}
			if cleanup == altExit {
				cleanupAt := bytes.Index(writes[cleanup], []byte("a=d,i="))
				altExitAt := bytes.Index(writes[altExit], []byte(ansi.ResetModeAltScreenSaveCursor))
				if cleanupAt < 0 || altExitAt < 0 || cleanupAt >= altExitAt {
					t.Fatalf("same-write abnormal cleanup order cleanup=%d alt-exit=%d bytes=%q", cleanupAt, altExitAt, writes[cleanup])
				}
			}
		})
	}
}

type failPostFrameCapture struct {
	capture   *testutil.ByteCapture
	err       error
	mu        sync.Mutex
	recovered bool
	attempts  int
}

func (w *failPostFrameCapture) Write(p []byte) (int, error) {
	_, _ = w.capture.Write(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes.Contains(p, []byte("a=p")) {
		w.attempts++
		if !w.recovered {
			return 0, w.err
		}
	}
	return len(p), nil
}

func (w *failPostFrameCapture) recover() {
	w.mu.Lock()
	w.recovered = true
	w.mu.Unlock()
}

func (w *failPostFrameCapture) attemptCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attempts
}

func TestRealBubbleTeaPersistentPostFrameFailurePausesUntilNextGeneration(t *testing.T) {
	requireFullTestSuite(t)
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	probe := newImagePipelineProbe(t, probeImageFrame)
	probe.model.tracker = ui.NewToolTracker()
	probe.model.tracker.AddImageSegment(probe.path)
	probe.model.streaming = true
	probe.model.bumpContentVersion()

	postFrameErr := errors.New("injected persistent post-frame failure")
	writer := &failPostFrameCapture{capture: testutil.NewByteCapture(), err: postFrameErr}
	probe.onRetry = writer.recover
	probe.script = delayedProbeMsg(180*time.Millisecond, probeRetryImagesMsg{})
	programOpts := []tea.ProgramOption{
		tea.WithInput(bytes.NewReader(nil)),
		tea.WithOutput(writer),
		tea.WithWindowSize(60, 18),
		tea.WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
		tea.WithoutSignalHandler(),
		tea.WithFPS(120),
	}
	program := tea.NewProgram(probe, programOpts...)
	probe.model.SetProgram(program)
	if _, err := program.Run(); err != nil {
		t.Fatalf("successful renderer frame became a program error: %v", err)
	}

	writes := writer.capture.Writes()
	retryFrame := testWriteContaining(writes, probeRetryFrame, 0)
	if retryFrame < 0 {
		t.Fatalf("next-generation retry frame missing: %q", writes)
	}
	if got := len(testWritesContaining(writes, "a=p", 0, retryFrame)); got != 1 {
		t.Fatalf("persistent failure placement attempts before refresh = %d, want 1; writes=%q", got, writes[:retryFrame+1])
	}
	if got := len(testWritesContaining(writes, "Terminal image update failed", 0, retryFrame)); got != 1 {
		t.Fatalf("persistent failure footer count = %d, want 1", got)
	}
	if recoveredPlacement := testWriteContaining(writes, "a=p", retryFrame+1); recoveredPlacement < 0 {
		t.Fatalf("next generation did not recover after writer recovery: %q", writes[retryFrame:])
	}
	if got := writer.attemptCount(); got != 2 {
		t.Fatalf("total post-frame placement attempts = %d, want failed once plus recovered once", got)
	}
}
