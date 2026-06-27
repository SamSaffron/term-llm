package serve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestTelegramStoreOpQueueFullDegradesWithoutBlocking(t *testing.T) {
	mgr := &telegramSessionMgr{store: &session.NoopStore{}}
	q := newTelegramStoreOpQueue(mgr, "session-1")

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var ran atomic.Int32

	if !q.enqueue(context.Background(), "first", func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		ran.Add(1)
		return nil
	}) {
		t.Fatal("first enqueue failed")
	}

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first op did not start")
	}

	for i := 0; i < 128; i++ {
		if !q.enqueue(context.Background(), "buffered", func(context.Context) error {
			ran.Add(1)
			return nil
		}) {
			t.Fatalf("buffered enqueue %d failed before queue was full", i)
		}
	}

	finalRan := atomic.Bool{}
	done := make(chan bool, 1)
	go func() {
		done <- q.enqueue(context.Background(), "final", func(context.Context) error {
			finalRan.Store(true)
			return nil
		})
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("full queue enqueue unexpectedly succeeded")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("full queue enqueue blocked")
	}
	if !q.isDegraded() {
		t.Fatal("queue was not marked degraded after saturation")
	}
	if finalRan.Load() {
		t.Fatal("degraded enqueue ran callback")
	}

	close(releaseFirst)
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !q.closeAndWait(drainCtx) {
		t.Fatal("queue did not drain")
	}
	if ran.Load() != 1 {
		t.Fatalf("degraded queue drained buffered callbacks; ran=%d want only first callback", ran.Load())
	}
}

func TestTelegramStoreOpQueueCanceledContextDegradesWithoutBlocking(t *testing.T) {
	mgr := &telegramSessionMgr{store: &session.NoopStore{}}
	q := newTelegramStoreOpQueue(mgr, "session-1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ok := q.enqueue(ctx, "canceled", func(context.Context) error {
		t.Fatal("canceled enqueue callback ran")
		return nil
	})
	if ok {
		t.Fatal("canceled enqueue succeeded")
	}
	if !q.isDegraded() {
		t.Fatal("queue was not marked degraded after canceled enqueue")
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if !q.closeAndWait(drainCtx) {
		t.Fatal("queue did not drain")
	}
}

func TestTelegramStoreOpQueueCloseSemantics(t *testing.T) {
	mgr := &telegramSessionMgr{store: &session.NoopStore{}}
	q := newTelegramStoreOpQueue(mgr, "session-1")

	var ran atomic.Bool
	if !q.enqueue(context.Background(), "op", func(context.Context) error {
		ran.Store(true)
		return nil
	}) {
		t.Fatal("enqueue failed")
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !q.closeAndWait(drainCtx) {
		t.Fatal("queue did not drain")
	}
	if !ran.Load() {
		t.Fatal("queued op did not run before close")
	}
	if q.enqueue(context.Background(), "after-close", func(context.Context) error {
		t.Fatal("closed queue callback ran")
		return nil
	}) {
		t.Fatal("enqueue after close succeeded")
	}
}
