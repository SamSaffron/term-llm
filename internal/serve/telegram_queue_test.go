package serve

import (
	"context"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestTelegramStoreOpQueueFullStillSerializesOps(t *testing.T) {
	mgr := &telegramSessionMgr{store: &session.NoopStore{}}
	q := newTelegramStoreOpQueue(mgr, "session-1")

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	finalRan := make(chan struct{})
	finalEnqueued := make(chan struct{})

	q.enqueue(context.Background(), "first", func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		return nil
	})

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first op did not start")
	}

	for i := 0; i < 128; i++ {
		q.enqueue(context.Background(), "buffered", func(context.Context) error {
			return nil
		})
	}

	go func() {
		q.enqueue(context.Background(), "final", func(context.Context) error {
			close(finalRan)
			return nil
		})
		close(finalEnqueued)
	}()

	select {
	case <-finalEnqueued:
		t.Fatal("final enqueue returned while queue was full")
	case <-time.After(100 * time.Millisecond):
	}

	select {
	case <-finalRan:
		t.Fatal("final op ran before queued ops drained")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)

	select {
	case <-finalEnqueued:
	case <-time.After(5 * time.Second):
		t.Fatal("final enqueue did not complete after queue space freed")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q.closeAndWait(drainCtx)

	select {
	case <-finalRan:
	default:
		t.Fatal("final op did not finish before closeAndWait returned")
	}
}
