package serve

import (
	"context"
	"testing"
)

func TestHubReverseCancellationBacklogBound(t *testing.T) {
	s, _, node, _, c := containmentHub(t)
	c.writeGate <- struct{}{}
	defer func() { <-c.writeGate }()
	// At most one fixed cancel worker may wait on the occupied writer, with a
	// finite queue behind it. Overflow terminates only this node's tunnel.
	for i := 0; i < hubReverseMaxActive+2; i++ {
		c.queueCancel("request")
	}
	if len(c.cancels) > hubReverseMaxActive {
		t.Fatalf("cancel backlog=%d", len(c.cancels))
	}
	if s.reverse.isConnected(node.ID) {
		t.Fatal("overflowed cancel queue kept tunnel connected")
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
