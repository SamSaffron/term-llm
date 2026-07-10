package run

import (
	"context"
	"testing"
	"time"
)

func TestWaitForRunnerDoneReturnsWhenDoneCloses(t *testing.T) {
	done := make(chan struct{})
	close(done)

	if !WaitForRunnerDone(context.Background(), done, time.Second) {
		t.Fatal("WaitForRunnerDone returned false for closed done channel")
	}
}

func TestWaitForRunnerDoneTimesOut(t *testing.T) {
	done := make(chan struct{})
	start := time.Now()

	if WaitForRunnerDone(context.Background(), done, 10*time.Millisecond) {
		t.Fatal("WaitForRunnerDone returned true for blocked runner")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("WaitForRunnerDone took %s, want bounded timeout", elapsed)
	}
}

func TestWaitForRunnerDoneAllowsCleanupAfterCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(done)
	}()

	if !WaitForRunnerDone(ctx, done, time.Second) {
		t.Fatal("WaitForRunnerDone did not allow cleanup after caller context cancellation")
	}
}
