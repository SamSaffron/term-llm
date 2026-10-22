package greprerank

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func waitSearchWorkers(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("search workers did not exit; active=%d", len(searchWorkers))
		}
		runtime.Gosched()
	}
}

func TestSearchWorkerAdmissionRetainedUntilActualExit(t *testing.T) {
	waitSearchWorkers(t, func() bool { return len(searchWorkers) == 0 })
	gates := make([]chan struct{}, MaxSearchWorkers)
	defer func() {
		for _, gate := range gates {
			if gate != nil {
				close(gate)
			}
		}
		waitSearchWorkers(t, func() bool { return len(searchWorkers) == 0 })
	}()
	var entered, active, maximum atomic.Int32
	for i := range gates {
		gate := make(chan struct{})
		gates[i] = gate
		// Separate rerankers model concurrent registries/sessions. Admission is global.
		r := newRR(okBackend(), Options{Budget: 20 * time.Millisecond})
		attempt, err := r.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, err := RunAttempt(context.Background(), attempt, func(_ context.Context, publish func(string)) (string, error) {
			entered.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for old := maximum.Load(); current > old; old = maximum.Load() {
				if maximum.CompareAndSwap(old, current) {
					break
				}
			}
			publish("ordinary prefix")
			<-gate // ignores cancellation, just like an uninterruptible filesystem call
			return "expanded", nil
		}, func(context.Context, string) (string, error) { return "ranked", nil }, func(prefix string) string { return prefix })
		if !errors.Is(err, context.DeadlineExceeded) || got != "ordinary prefix" {
			t.Fatalf("call %d text=%q err=%v", i, got, err)
		}
	}
	if active.Load() != MaxSearchWorkers || maximum.Load() != MaxSearchWorkers {
		t.Fatalf("active=%d maximum=%d", active.Load(), maximum.Load())
	}
	// Timing out the four callers must not free their still-running workers.
	busyReranker := newRR(okBackend(), Options{Budget: time.Second})
	for range breakerFailures + 2 {
		attempt, err := busyReranker.Begin(context.Background())
		if err != nil {
			t.Fatalf("busy skips tripped breaker: %v", err)
		}
		type outcome struct {
			text string
			err  error
		}
		result := make(chan outcome, 1)
		go func() {
			text, err := RunAttempt(context.Background(), attempt, func(context.Context, func(string)) (string, error) { entered.Add(1); return "unexpected", nil }, func(context.Context, string) (string, error) { return "unexpected", nil }, func(string) string { t.Error("busy admission has no ordinary prefix"); return "unexpected" })
			result <- outcome{text, err}
		}()
		select {
		case got := <-result:
			if got.text != "" || !errors.Is(got.err, ErrBusy) {
				t.Fatalf("admission result=%+v", got)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("saturated admission blocked instead of skipping")
		}
	}
	if entered.Load() != MaxSearchWorkers {
		t.Fatalf("saturation launched %d workers", entered.Load())
	}
	// Releasing an actual worker, not its timed-out caller, restores admission.
	close(gates[0])
	gates[0] = nil
	waitSearchWorkers(t, func() bool { return len(searchWorkers) < MaxSearchWorkers })
	attempt, err := busyReranker.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := RunAttempt(context.Background(), attempt, func(context.Context, func(string)) (string, error) { return "ordinary", nil }, func(context.Context, string) (string, error) { return "ranked", nil }, func(prefix string) string { return prefix })
	if err != nil || got != "ranked" {
		t.Fatalf("admission did not recover: %q %v", got, err)
	}
}

func TestSearchWorkerAdmissionHonorsCanceledParent(t *testing.T) {
	r := newRR(okBackend(), Options{})
	parent, cancel := context.WithCancel(context.Background())
	attempt, err := r.Begin(parent)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = RunAttempt(parent, attempt, func(context.Context, func(string)) (string, error) {
		t.Error("canceled parent started a worker")
		return "", nil
	}, func(context.Context, string) (string, error) { return "", nil }, func(s string) string { return s })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSearchWorkerAdmissionRejectsExpiredAttempt(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: time.Millisecond})
	attempt, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	attempt.Start()
	<-attempt.Context().Done()
	_, err = RunAttempt(context.Background(), attempt, func(context.Context, func(string)) (string, error) {
		t.Error("expired attempt started a worker")
		return "", nil
	}, func(context.Context, string) (string, error) { return "", nil }, func(s string) string { return s })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
