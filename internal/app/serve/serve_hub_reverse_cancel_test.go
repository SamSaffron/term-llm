package serve

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func waitForReverseFrameType(t *testing.T, frames <-chan hubReverseRequest, frameType string) hubReverseRequest {
	t.Helper()
	watchdog := time.After(5 * time.Second)
	for {
		select {
		case frame := <-frames:
			if frame.Type == frameType {
				return frame
			}
		case <-watchdog:
			t.Fatalf("no %s frame reached the node", frameType)
			return hubReverseRequest{}
		}
	}
}

// Until its cancel frame is written the node may still be running a canceled
// operation, so the operation keeps its admission slot until then (or until
// the tunnel closes).
func TestHubReverseCancelHoldsSlotUntilWritten(t *testing.T) {
	for _, ending := range []string{"written", "close"} {
		t.Run(ending, func(t *testing.T) {
			s, _, node, peer, c := containmentHub(t)
			frames := make(chan hubReverseRequest, 8)
			go func() {
				for {
					var frame hubReverseRequest
					if peer.ReadJSON(&frame) != nil {
						return
					}
					frames <- frame
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			done := containmentDo(s, ctx, node, "/slow", nil)
			req := waitForReverseFrameType(t, frames, hubReverseFrameRequest)
			c.writeGate <- struct{}{} // occupy the writer: the cancel cannot go out yet
			cancel()
			if err := containmentAwait(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled call=%v", err)
			}
			if len(c.active) != 1 {
				t.Fatalf("slot released before the node heard the cancel: active=%d", len(c.active))
			}
			switch ending {
			case "written":
				<-c.writeGate
				if got := waitForReverseFrameType(t, frames, hubReverseFrameCancel); got.ID != req.ID {
					t.Fatalf("cancel for %q, want %q", got.ID, req.ID)
				}
			case "close":
				c.closeWithError(errors.New("test tunnel shutdown"))
				<-c.writeGate
			}
			waitForHubAdmission(t, c, 0, 0)
		})
	}
}

// Abandoning a queued burst (reload or navigation mid-load) cancels requests as
// they reach the node. Freed slots must not let queued callers outrun those
// cancels: the node never sees more than hubReverseMaxActive live operations,
// and the cancel backlog cannot overflow and drop the whole tunnel.
func TestHubReverseAbandonedBurstKeepsTunnel(t *testing.T) {
	s, _, node, peer, c := containmentHub(t)
	const n = hubReverseMaxActive + hubReverseMaxWaiting
	arrived := make(chan string, n)
	var live, maxLive atomic.Int64
	go func() {
		for {
			var frame hubReverseRequest
			if peer.ReadJSON(&frame) != nil {
				return
			}
			switch frame.Type {
			case hubReverseFrameRequest:
				if v := live.Add(1); v > maxLive.Load() {
					maxLive.Store(v)
				}
				arrived <- frame.Path
			case hubReverseFrameCancel:
				live.Add(-1)
			}
		}
	}()
	cancels := make(map[string]context.CancelFunc, n)
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	results := make(chan error, n)
	for i := range n {
		ctx, cancel := context.WithCancel(t.Context())
		path := fmt.Sprintf("/asset-%d", i)
		cancels[path] = cancel
		call := containmentDo(s, ctx, node, path, nil)
		go func() { results <- <-call }()
	}
	waitForHubAdmission(t, c, hubReverseMaxActive, hubReverseMaxWaiting)
	watchdog := time.After(10 * time.Second)
	for finished := 0; finished < n; {
		select {
		case path := <-arrived:
			cancels[path]()
		case err := <-results:
			finished++
			if !errors.Is(err, context.Canceled) && !errors.Is(err, errHubReverseBusy) {
				t.Fatalf("abandoned request failed with %v", err)
			}
		case <-watchdog:
			t.Fatalf("abandoned burst did not drain: %d/%d", finished, n)
		}
	}
	select {
	case <-c.closed:
		t.Fatal("abandoning a queued burst tore down the node tunnel")
	default:
	}
	if got := maxLive.Load(); got > hubReverseMaxActive {
		t.Fatalf("node saw %d live operations, want <= %d", got, hubReverseMaxActive)
	}
	waitForHubAdmission(t, c, 0, 0)
	c.pendingMu.Lock()
	pending := len(c.pending)
	c.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("retained %d pending operations", pending)
	}
}
