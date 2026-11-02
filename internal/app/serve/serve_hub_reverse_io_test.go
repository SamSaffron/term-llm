package serve

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestHubReverseCancellationBacklogBound(t *testing.T) {
	s, _, node, _, c := containmentHub(t)
	c.writeGate <- struct{}{}
	defer func() { <-c.writeGate }()
	// At most one fixed cancel worker may wait on the occupied writer, with a
	// finite queue behind it. Overflow terminates only this node's tunnel, and
	// every queued cancel still returns the slot it holds.
	var released atomic.Int64
	n := cap(c.cancels.Queue()) + 2
	for range n {
		c.queueCancel("request", func() { released.Add(1) })
	}
	if s.reverse.isConnected(node.ID) {
		t.Fatal("overflowed cancel queue kept tunnel connected")
	}
	watchdog := time.After(5 * time.Second)
	for released.Load() != int64(n) {
		select {
		case <-watchdog:
			t.Fatalf("released %d of %d cancel slots", released.Load(), n)
		default:
			runtime.Gosched()
		}
	}
}

func TestHubReverseWriteFailureDrainsPending(t *testing.T) {
	s, _, node, peer, c := containmentHub(t)
	first := containmentStall(t, s, node, c, context.Background())
	_ = peer.Close()
	if err := containmentAwait(t, first); err == nil {
		t.Fatal("failed socket write succeeded")
	}
	if s.reverse.isConnected(node.ID) {
		t.Fatal("failed connection remains connected")
	}
	c.pendingMu.Lock()
	count := len(c.pending)
	c.pendingMu.Unlock()
	if count != 0 {
		t.Fatalf("failed connection retained %d pending operations", count)
	}
	if len(c.active) != 0 {
		t.Fatalf("failed connection retained %d admissions", len(c.active))
	}
}
