package server

import "testing"

func TestReverseSlotMoveTo(t *testing.T) {
	requests, streams := make(chan struct{}, 2), make(chan struct{}, 1)
	hold := func(s *ReverseSlot) {
		requests <- struct{}{}
		s.Hold(func() { <-requests })
	}
	var moved, stuck ReverseSlot
	hold(&moved)
	if !moved.MoveTo(streams) || len(requests) != 0 || len(streams) != 1 {
		t.Fatalf("move: requests=%d streams=%d", len(requests), len(streams))
	}
	hold(&stuck)
	if stuck.MoveTo(streams) || len(requests) != 1 || len(streams) != 1 {
		t.Fatalf("full pool moved a token: requests=%d streams=%d", len(requests), len(streams))
	}
	stuck.Release()
	moved.Release()
	moved.Release() // idempotent
	if len(requests) != 0 || len(streams) != 0 {
		t.Fatalf("release: requests=%d streams=%d", len(requests), len(streams))
	}
	if moved.MoveTo(streams) || len(streams) != 0 {
		t.Fatal("returned token moved")
	}
}

func TestReverseCancelsReturnEverySlot(t *testing.T) {
	q := NewReverseCancels(1)
	released := 0
	cancel := ReverseCancel{ID: "op", Release: func() { released++ }}
	if !q.Push(cancel) {
		t.Fatal("push into empty backlog failed")
	}
	if q.Push(cancel) || released != 0 {
		t.Fatalf("full backlog must leave the slot with the caller: released=%d", released)
	}
	q.Close() // returns the queued, never-written cancel's slot
	if !q.Push(cancel) || released != 2 {
		t.Fatalf("after close: released=%d, want 2", released)
	}
}
