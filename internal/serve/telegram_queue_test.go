package serve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestTelegramStoreOpQueueFullDropsNewOpWithoutBlockingEnginePath(t *testing.T) {
	mgr := &telegramSessionMgr{store: &session.NoopStore{}}
	q := newTelegramStoreOpQueue(mgr, "session-1")

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	finalRan := make(chan struct{})

	if !q.enqueue(context.Background(), "first", func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		return nil
	}) {
		t.Fatal("first enqueue unexpectedly failed")
	}

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first op did not start")
	}

	for i := 0; i < 128; i++ {
		if !q.enqueue(context.Background(), "buffered", func(context.Context) error { return nil }) {
			t.Fatalf("buffered enqueue %d unexpectedly failed", i)
		}
	}

	start := time.Now()
	if q.enqueue(context.Background(), "final", func(context.Context) error {
		close(finalRan)
		return nil
	}) {
		t.Fatal("final enqueue succeeded despite full queue")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("full queue enqueue blocked for %s", elapsed)
	}
	if !q.shouldReconcile() {
		t.Fatal("full queue should mark persistence for reconciliation")
	}

	close(releaseFirst)
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !q.closeAndWait(drainCtx) {
		t.Fatal("queue did not drain after releasing first op")
	}

	select {
	case <-finalRan:
		t.Fatal("dropped op ran despite degraded queue")
	default:
	}
}

func TestTelegramStoreOpQueueCanceledContextReturnsImmediately(t *testing.T) {
	mgr := &telegramSessionMgr{store: &session.NoopStore{}}
	q := newTelegramStoreOpQueue(mgr, "session-1")

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	if !q.enqueue(context.Background(), "first", func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		return nil
	}) {
		t.Fatal("first enqueue unexpectedly failed")
	}

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first op did not start")
	}

	for i := 0; i < 128; i++ {
		if !q.enqueue(context.Background(), "buffered", func(context.Context) error { return nil }) {
			t.Fatalf("buffered enqueue %d unexpectedly failed", i)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if q.enqueue(ctx, "canceled", func(context.Context) error { return nil }) {
		t.Fatal("enqueue succeeded despite canceled context and full queue")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled enqueue blocked for %s", elapsed)
	}

	close(releaseFirst)
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if !q.closeAndWait(drainCtx) {
		t.Fatal("queue did not drain after releasing first op")
	}
}

func TestTelegramStoreOpQueueCloseRejectsNewOps(t *testing.T) {
	mgr := &telegramSessionMgr{store: &session.NoopStore{}}
	q := newTelegramStoreOpQueue(mgr, "session-1")

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	if !q.enqueue(context.Background(), "first", func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		return nil
	}) {
		t.Fatal("first enqueue unexpectedly failed")
	}

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first op did not start")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if q.closeAndWait(closeCtx) {
		t.Fatal("closeAndWait reported a full drain while first op was blocked")
	}

	var ran atomic.Bool
	if q.enqueue(context.Background(), "after-close", func(context.Context) error {
		ran.Store(true)
		return nil
	}) {
		t.Fatal("enqueue after close unexpectedly succeeded")
	}

	close(releaseFirst)
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if !q.closeAndWait(drainCtx) {
		t.Fatal("queue did not drain after releasing first op")
	}
	if ran.Load() {
		t.Fatal("enqueue after close should never run")
	}
}
