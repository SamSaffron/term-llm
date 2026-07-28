package tea

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testRendererFrame = "<renderer-frame>"
	testPostFrame     = "<post-frame>"
	testRawOutput     = "<raw-output>"
	testCapability    = "<capability-output>"
	testScrollback    = "<scrollback-output>"
)

type boundaryWriter struct {
	mu           sync.Mutex
	writes       [][]byte
	failExact    []byte
	failContains []byte
	failErr      error
	failAfter    int
	failedOnce   bool
}

func (w *boundaryWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	copyOfP := bytes.Clone(p)
	w.writes = append(w.writes, copyOfP)
	if !w.failedOnce && ((len(w.failExact) > 0 && bytes.Equal(p, w.failExact)) ||
		(len(w.failContains) > 0 && bytes.Contains(p, w.failContains))) {
		w.failedOnce = true
		if w.failAfter > 0 && w.failAfter < len(p) {
			return w.failAfter, w.failErr
		}
		return 0, w.failErr
	}
	return len(p), nil
}

func (w *boundaryWriter) snapshot() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()

	writes := make([][]byte, len(w.writes))
	for i := range w.writes {
		writes[i] = bytes.Clone(w.writes[i])
	}
	return writes
}

func writeContaining(writes [][]byte, marker string, start int) int {
	for i := start; i < len(writes); i++ {
		if bytes.Contains(writes[i], []byte(marker)) {
			return i
		}
	}
	return -1
}

func newBoundaryRenderer(w *boundaryWriter) *cursedRenderer {
	return newCursedRenderer(w, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, 80, 24)
}

func TestPostFrameFollowsItsRendererFrameNotUnmanagedOutput(t *testing.T) {
	output := &boundaryWriter{}
	renderer := newBoundaryRenderer(output)
	completed := make(chan error, 1)
	view := NewView(testRendererFrame)
	view.PostFrame = testPostFrame
	view.PostFrameMsg = func(err error) Msg { completed <- err; return nil }
	renderer.render(view)

	// Raw and capability commands are queued on Program.outputBuf and written
	// by Program.flush. insertAbove synchronizes the queued frame first so its
	// geometry cannot weld a stale row into scrollback; PostFrame remains bound
	// immediately after that exact frame.
	program := &Program{output: output}
	program.execute(testRawOutput)
	program.execute(testCapability)
	if err := program.flush(); err != nil {
		t.Fatalf("program flush error = %v", err)
	}
	if err := renderer.insertAbove(testScrollback); err != nil {
		t.Fatalf("scrollback insert error = %v", err)
	}
	if err := renderer.flush(false); err != nil {
		t.Fatalf("renderer flush error = %v", err)
	}
	if err := <-completed; err != nil {
		t.Fatalf("PostFrameMsg error = %v", err)
	}

	writes := output.snapshot()
	rawWrite := writeContaining(writes, testRawOutput, 0)
	capabilityWrite := writeContaining(writes, testCapability, 0)
	scrollbackWrite := writeContaining(writes, testScrollback, 0)
	frameWrite := writeContaining(writes, testRendererFrame, 0)
	postFrameWrite := writeContaining(writes, testPostFrame, 0)
	if rawWrite < 0 || capabilityWrite < 0 || scrollbackWrite < 0 || frameWrite < 0 || postFrameWrite < 0 {
		t.Fatalf("missing writes raw=%d capability=%d scrollback=%d frame=%d post-frame=%d in %q", rawWrite, capabilityWrite, scrollbackWrite, frameWrite, postFrameWrite, writes)
	}
	if !(rawWrite <= capabilityWrite && capabilityWrite < frameWrite && frameWrite < postFrameWrite && postFrameWrite < scrollbackWrite) {
		t.Fatalf("write order raw=%d capability=%d frame=%d post-frame=%d scrollback=%d, want unmanaged output then synchronized frame/post-frame then scrollback", rawWrite, capabilityWrite, frameWrite, postFrameWrite, scrollbackWrite)
	}
}

func TestPostFrameRapidReplacementKeepsExactAcceptedView(t *testing.T) {
	output := &boundaryWriter{}
	renderer := newBoundaryRenderer(output)
	var replacedCallbacks atomic.Int32
	acceptedDone := make(chan error, 1)

	replacedPayload := "<replaced-post-frame>"
	replaced := NewView("<replaced-frame>")
	replaced.PostFrame = replacedPayload
	replaced.PostFrameMsg = func(error) Msg { replacedCallbacks.Add(1); return nil }
	renderer.render(replaced)

	acceptedPayload := testPostFrame
	accepted := NewView(testRendererFrame)
	accepted.PostFrame = acceptedPayload
	accepted.PostFrameMsg = func(err error) Msg { acceptedDone <- err; return nil }
	renderer.render(accepted)

	if err := renderer.flush(false); err != nil {
		t.Fatalf("renderer flush error = %v", err)
	}
	if err := <-acceptedDone; err != nil {
		t.Fatalf("accepted completion error = %v", err)
	}
	if got := replacedCallbacks.Load(); got != 0 {
		t.Fatalf("replaced completion count = %d, want 0", got)
	}
	writes := output.snapshot()
	if writeContaining(writes, "<replaced-frame>", 0) >= 0 || writeContaining(writes, "<replaced-post-frame>", 0) >= 0 {
		t.Fatalf("replaced View reached output: %q", writes)
	}
	if writeContaining(writes, testRendererFrame, 0) < 0 || writeContaining(writes, testPostFrame, 0) < 0 {
		t.Fatalf("accepted View missing from output: %q", writes)
	}
}

func TestPostFrameRapidIdenticalReplacementCompletesOnlyNewestView(t *testing.T) {
	output := &boundaryWriter{}
	renderer := newBoundaryRenderer(output)
	renderer.render(NewView(testRendererFrame))
	if err := renderer.flush(false); err != nil {
		t.Fatalf("initial renderer flush error = %v", err)
	}

	var replacedCallbacks atomic.Int32
	replaced := NewView(testRendererFrame)
	replaced.PostFrame = "<identical-replaced-post-frame>"
	replaced.PostFrameMsg = func(error) Msg { replacedCallbacks.Add(1); return nil }
	renderer.render(replaced)

	acceptedDone := make(chan error, 1)
	accepted := NewView(testRendererFrame)
	accepted.PostFrame = testPostFrame
	accepted.PostFrameMsg = func(err error) Msg { acceptedDone <- err; return nil }
	renderer.render(accepted)

	if err := renderer.flush(false); err != nil {
		t.Fatalf("unchanged renderer flush error = %v", err)
	}
	if err := <-acceptedDone; err != nil {
		t.Fatalf("accepted completion error = %v", err)
	}
	if got := replacedCallbacks.Load(); got != 0 {
		t.Fatalf("identical replaced completion count = %d, want 0", got)
	}
	writes := output.snapshot()
	if writeContaining(writes, "<identical-replaced-post-frame>", 0) >= 0 || writeContaining(writes, testPostFrame, 0) < 0 {
		t.Fatalf("identical replacement emitted wrong payload: %q", writes)
	}
}

func TestPostFrameFlushesOnceForUnchangedFrameAndCommitsExactView(t *testing.T) {
	output := &boundaryWriter{}
	renderer := newBoundaryRenderer(output)
	renderer.render(NewView(testRendererFrame))
	if err := renderer.flush(false); err != nil {
		t.Fatalf("initial renderer flush error = %v", err)
	}

	initialWrites := len(output.snapshot())
	completed := make(chan error, 1)
	unchanged := NewView(testRendererFrame)
	unchanged.OnMouse = func(MouseMsg) Cmd { return nil }
	unchanged.PostFrame = testPostFrame
	unchanged.PostFrameMsg = func(err error) Msg { completed <- err; return nil }
	renderer.render(unchanged)
	if err := renderer.flush(false); err != nil {
		t.Fatalf("unchanged renderer flush error = %v", err)
	}
	if err := <-completed; err != nil {
		t.Fatalf("unchanged completion error = %v", err)
	}

	writes := output.snapshot()
	newWrites := writes[initialWrites:]
	if len(newWrites) != 1 || !bytes.Equal(newWrites[0], []byte(testPostFrame)) {
		t.Fatalf("unchanged frame writes = %q, want one post-frame write", newWrites)
	}
	if !renderer.hasLastView || renderer.lastView.OnMouse == nil {
		t.Fatalf("unchanged accepted View was not committed: %#v", renderer.lastView)
	}
	if len(renderer.lastView.PostFrame) != 0 || renderer.lastView.PostFrameMsg != nil {
		t.Fatalf("lastView retained consumed post-frame state: %#v", renderer.lastView)
	}

	if err := renderer.flush(false); err != nil {
		t.Fatalf("repeat renderer flush error = %v", err)
	}
	if got := len(output.snapshot()); got != len(writes) {
		t.Fatalf("post-frame was emitted again: write count = %d, want %d", got, len(writes))
	}
}

func TestPostFrameSurvivesResizeBeforeFlush(t *testing.T) {
	output := &boundaryWriter{}
	renderer := newBoundaryRenderer(output)
	completed := make(chan error, 1)
	view := NewView(testRendererFrame)
	view.PostFrame = testPostFrame
	view.PostFrameMsg = func(err error) Msg { completed <- err; return nil }
	renderer.render(view)
	renderer.resize(40, 12)
	if err := renderer.flush(false); err != nil {
		t.Fatalf("renderer flush after resize error = %v", err)
	}
	if err := <-completed; err != nil {
		t.Fatalf("completion after resize error = %v", err)
	}
	writes := output.snapshot()
	frameWrite := writeContaining(writes, testRendererFrame, 0)
	postFrameWrite := writeContaining(writes, testPostFrame, 0)
	if frameWrite < 0 || postFrameWrite <= frameWrite {
		t.Fatalf("resize detached side band: frame=%d post-frame=%d writes=%q", frameWrite, postFrameWrite, writes)
	}
}

func TestFrameErrorDiscardsPostFrameAndCompletion(t *testing.T) {
	frameErr := errors.New("frame write failed")
	output := &boundaryWriter{failContains: []byte(testRendererFrame), failErr: frameErr}
	renderer := newBoundaryRenderer(output)
	var callbacks atomic.Int32
	view := NewView(testRendererFrame)
	view.PostFrame = testPostFrame
	view.PostFrameMsg = func(error) Msg { callbacks.Add(1); return nil }
	renderer.render(view)

	if err := renderer.flush(false); !errors.Is(err, frameErr) {
		t.Fatalf("renderer flush error = %v, want %v", err, frameErr)
	}
	if write := writeContaining(output.snapshot(), testPostFrame, 0); write >= 0 {
		t.Fatalf("failed frame emitted post-frame payload at write %d", write)
	}

	// The renderer may retry its own state, but this exact frame's side-band
	// state must not attach to that later attempt.
	if err := renderer.flush(false); err != nil {
		t.Fatalf("renderer retry error = %v", err)
	}
	if write := writeContaining(output.snapshot(), testPostFrame, 0); write >= 0 {
		t.Fatalf("failed frame's post-frame payload was retried at write %d", write)
	}
	if got := callbacks.Load(); got != 0 {
		t.Fatalf("failed frame completion count = %d, want 0", got)
	}
}

func TestFailedIncrementalFrameForcesCorrectFullRedraw(t *testing.T) {
	const width, height = 30, 6
	oldContent := strings.Join([]string{"header", "row-a", "row-b", "same", "same", "footer"}, "\n")
	failedContent := strings.Join([]string{"header", "failed-row-x", "row-b", "same", "same", "footer"}, "\n")
	recoveredContent := strings.Join([]string{"header", "row-a", "recovered-row-y", "same", "same", "footer"}, "\n")
	frameErr := errors.New("injected incremental frame failure")
	output := &boundaryWriter{failContains: []byte("failed-row-x"), failErr: frameErr}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, width, height)

	for _, content := range []string{oldContent, failedContent} {
		view := NewView(content)
		view.AltScreen = true
		r.render(view)
		err := r.flush(false)
		if content == failedContent {
			if !errors.Is(err, frameErr) {
				t.Fatalf("failed incremental flush error = %v, want %v", err, frameErr)
			}
		} else if err != nil {
			t.Fatalf("initial flush: %v", err)
		}
	}

	view := NewView(recoveredContent)
	view.AltScreen = true
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	assertRendererCellsEqualContent(t, r, recoveredContent, width, height)
}

func TestPostFrameErrorPreservesFrameBookkeepingAndCompletionRunsUnlocked(t *testing.T) {
	postFrameErr := errors.New("post-frame write failed")
	output := &boundaryWriter{failExact: []byte(testPostFrame), failErr: postFrameErr}
	renderer := newBoundaryRenderer(output)
	callbackDone := make(chan error, 1)

	view := NewView(testRendererFrame)
	view.PostFrame = testPostFrame
	view.PostFrameMsg = func(err error) Msg {
		// render takes the renderer mutex. This deadlocks if the completion is
		// invoked before flush releases that mutex.
		renderer.render(NewView("<callback-frame>"))
		callbackDone <- err
		return nil
	}
	renderer.render(view)
	flushDone := make(chan error, 1)
	go func() {
		flushDone <- renderer.flush(false)
	}()

	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatalf("renderer flush returned side-band error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("renderer flush deadlocked in post-frame completion")
	}

	select {
	case err := <-callbackDone:
		if !errors.Is(err, postFrameErr) {
			t.Fatalf("post-frame completion error = %v, want %v", err, postFrameErr)
		}
	case <-time.After(time.Second):
		t.Fatal("post-frame completion was not invoked")
	}

	if !renderer.forceRedraw {
		t.Fatal("post-frame write failure did not invalidate incremental state")
	}
	if err := renderer.flush(false); err != nil {
		t.Fatalf("full redraw after post-frame failure: %v", err)
	}
	if renderer.forceRedraw {
		t.Fatal("successful recovery did not rebuild incremental state")
	}
	if !renderer.hasLastView || renderer.lastView.Content != "<callback-frame>" {
		t.Fatalf("lastView = %#v, want callback recovery frame", renderer.lastView)
	}
	if len(renderer.lastView.PostFrame) != 0 || renderer.lastView.PostFrameMsg != nil {
		t.Fatalf("lastView retained consumed post-frame state: %#v", renderer.lastView)
	}
}

func TestPostFramePartialWriteReportsOnceAndDoesNotRetry(t *testing.T) {
	postFrameErr := errors.New("partial post-frame write")
	output := &boundaryWriter{failExact: []byte(testPostFrame), failErr: postFrameErr, failAfter: 4}
	renderer := newBoundaryRenderer(output)
	completed := make(chan error, 2)
	view := NewView(testRendererFrame)
	view.PostFrame = testPostFrame
	view.PostFrameMsg = func(err error) Msg { completed <- err; return nil }
	renderer.render(view)

	if err := renderer.flush(false); err != nil {
		t.Fatalf("renderer flush returned side-band error = %v", err)
	}
	if err := <-completed; !errors.Is(err, postFrameErr) {
		t.Fatalf("partial-write completion error = %v, want %v", err, postFrameErr)
	}
	if err := renderer.flush(false); err != nil {
		t.Fatalf("repeat renderer flush error = %v", err)
	}
	select {
	case err := <-completed:
		t.Fatalf("partial payload completion repeated with %v", err)
	default:
	}
	if got := len(writeIndicesContaining(output.snapshot(), testPostFrame)); got != 1 {
		t.Fatalf("post-frame write attempts = %d, want 1", got)
	}
}

func TestPostFrameShortWriteWithoutErrorIsReported(t *testing.T) {
	output := &boundaryWriter{failExact: []byte(testPostFrame), failAfter: 4}
	renderer := newBoundaryRenderer(output)
	completed := make(chan error, 1)
	view := NewView(testRendererFrame)
	view.PostFrame = testPostFrame
	view.PostFrameMsg = func(err error) Msg { completed <- err; return nil }
	renderer.render(view)

	if err := renderer.flush(false); err != nil {
		t.Fatalf("renderer flush returned side-band error = %v", err)
	}
	if err := <-completed; !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short-write completion error = %v, want %v", err, io.ErrShortWrite)
	}
	if got := len(writeIndicesContaining(output.snapshot(), testPostFrame)); got != 1 {
		t.Fatalf("post-frame write attempts = %d, want 1", got)
	}
}

func TestProgramAndScrollbackReportShortWrites(t *testing.T) {
	t.Run("program output", func(t *testing.T) {
		output := &boundaryWriter{failExact: []byte(testRawOutput), failAfter: 4}
		renderer := newBoundaryRenderer(output)
		view := NewView(testRendererFrame)
		view.AltScreen = true
		renderer.render(view)
		if err := renderer.flush(false); err != nil {
			t.Fatal(err)
		}
		program := &Program{output: output, renderer: renderer}
		program.execute(testRawOutput)
		if err := program.flush(); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("flush error = %v, want %v", err, io.ErrShortWrite)
		}
		if !renderer.forceRedraw {
			t.Fatal("raw output failure did not invalidate renderer state")
		}
		renderer.render(view)
		if err := renderer.flush(false); err != nil {
			t.Fatalf("full redraw after raw output failure: %v", err)
		}
		if renderer.forceRedraw {
			t.Fatal("raw output recovery did not rebuild renderer state")
		}
	})

	t.Run("scrollback insertion", func(t *testing.T) {
		output := &boundaryWriter{}
		renderer := newBoundaryRenderer(output)
		renderer.render(NewView(testRendererFrame))
		if err := renderer.flush(false); err != nil {
			t.Fatal(err)
		}
		output.failContains = []byte(testScrollback)
		output.failAfter = 4
		if err := renderer.insertAbove(testScrollback); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("insertAbove error = %v, want %v", err, io.ErrShortWrite)
		}
	})
}

func TestRendererResultQueueDoesNotBlockTwoAcceptedFrames(t *testing.T) {
	output := &boundaryWriter{}
	renderer := newBoundaryRenderer(output)
	for i := 0; i < 2; i++ {
		view := NewView(testRendererFrame)
		view.PostFrame = testPostFrame
		view.PostFrameMsg = func(error) Msg { return rendererLifecycleAckMsg{} }
		renderer.render(view)
		flushed := make(chan error, 1)
		go func() { flushed <- renderer.flush(false) }()
		select {
		case err := <-flushed:
			if err != nil {
				t.Fatalf("flush %d: %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("flush %d blocked on renderer result scheduling", i)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-renderer.messages():
		default:
			t.Fatalf("renderer result %d was lost", i)
		}
	}
}

func TestRendererResultQueueFullNeverBlocksFlushInsertAboveOrShutdown(t *testing.T) {
	output := &boundaryWriter{}
	renderer := newBoundaryRenderer(output)
	for range cap(renderer.postFrameMsgs) {
		renderer.postFrameMsgs <- rendererLifecycleAckMsg{}
	}

	view := NewView(testRendererFrame)
	view.PostFrame = testPostFrame
	view.PostFrameMsg = func(error) Msg { return rendererLifecycleAckMsg{} }
	renderer.render(view)
	flushed := make(chan error, 1)
	go func() { flushed <- renderer.flush(false) }()
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatalf("queue-full flush: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue-full flush blocked")
	}

	view.PostFrame = testPostFrame
	renderer.render(view)
	inserted := make(chan error, 1)
	go func() { inserted <- renderer.insertAbove(testScrollback) }()
	select {
	case err := <-inserted:
		if err != nil {
			t.Fatalf("queue-full insertAbove: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue-full insertAbove blocked")
	}

	closed := make(chan error, 1)
	go func() { closed <- renderer.close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("queue-full close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue-full shutdown blocked")
	}
}

func writeIndicesContaining(writes [][]byte, marker string) []int {
	var indices []int
	for i := range writes {
		if bytes.Contains(writes[i], []byte(marker)) {
			indices = append(indices, i)
		}
	}
	return indices
}

func TestRendererShutdownNeverInvokesPostFrameCompletion(t *testing.T) {
	t.Run("graceful final flush writes without callback", func(t *testing.T) {
		output := &boundaryWriter{}
		renderer := newBoundaryRenderer(output)
		var callbacks atomic.Int32
		view := NewView(testRendererFrame)
		view.PostFrame = testPostFrame
		view.PostFrameMsg = func(error) Msg { callbacks.Add(1); return nil }
		renderer.render(view)
		if err := renderer.flush(true); err != nil {
			t.Fatalf("closing flush error = %v", err)
		}
		if writeContaining(output.snapshot(), testPostFrame, 0) < 0 {
			t.Fatal("graceful final flush did not write post-frame payload")
		}
		if got := callbacks.Load(); got != 0 {
			t.Fatalf("closing flush completion count = %d, want 0", got)
		}
	})

	t.Run("close discards unflushed state", func(t *testing.T) {
		output := &boundaryWriter{}
		renderer := newBoundaryRenderer(output)
		var callbacks atomic.Int32
		view := NewView(testRendererFrame)
		view.PostFrame = testPostFrame
		view.PostFrameMsg = func(error) Msg { callbacks.Add(1); return nil }
		renderer.render(view)
		if err := renderer.close(); err != nil {
			t.Fatalf("renderer close error = %v", err)
		}
		if write := writeContaining(output.snapshot(), testPostFrame, 0); write >= 0 {
			t.Fatalf("teardown emitted unflushed post-frame payload at write %d", write)
		}
		if got := callbacks.Load(); got != 0 {
			t.Fatalf("close completion count = %d, want 0", got)
		}
		if len(renderer.view.PostFrame) != 0 || renderer.view.PostFrameMsg != nil {
			t.Fatalf("close retained queued post-frame state: %#v", renderer.view)
		}
	})
}
