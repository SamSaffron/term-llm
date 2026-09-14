package tea

import (
	"fmt"
	"testing"
	"time"
)

// TestSetNoInputConcurrentWithFlush pins down the concurrency contract of the
// renderer setters: noInput is read by whichever goroutine is flushing frames
// (start, flush, and close all consult it while holding s.mu), so the setter
// must publish it under the same lock. Run under -race; without the lock the
// unsynchronized write to noInput races with the locked reads and the detector
// reports it.
func TestSetNoInputConcurrentWithFlush(t *testing.T) {
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, 20, 4)

	r.start()
	r.render(NewView("frame 0"))
	if err := r.flush(false); err != nil {
		t.Fatalf("initial flush: %v", err)
	}

	// Bounded: the flusher stops as soon as the setter loop is done, and it can
	// never run more than maxFrames frames.
	const maxFrames = 2000
	stop := make(chan struct{})
	flushed := make(chan error, 1)
	go func() {
		// Each frame differs from the last, so every flush takes the full
		// update path and re-reads noInput through
		// updateKeyboardEnhancementsLocked instead of taking the unchanged-frame
		// fast path.
		for i := 1; i <= maxFrames; i++ {
			select {
			case <-stop:
				flushed <- nil
				return
			default:
			}
			r.render(NewView(fmt.Sprintf("frame %d", i)))
			if err := r.flush(false); err != nil {
				flushed <- err
				return
			}
		}
		flushed <- nil
	}()

	for i := 0; i < 4; i++ {
		r.setNoInput(i%2 == 0)
		// Long enough for the flusher to observe the new value, short enough
		// that the whole test stays in the millisecond range.
		time.Sleep(2 * time.Millisecond)
	}
	close(stop)

	select {
	case err := <-flushed:
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("flushing goroutine did not stop")
	}
}
