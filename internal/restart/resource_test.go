package restart

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestResourceChecksPrecedeAllPreparation(t *testing.T) {
	c := &Coordinator{Timeout: time.Second}
	var prepared atomic.Int32
	c.Register(&Resource{Prepare: func(context.Context) (func(context.Context), error) { prepared.Add(1); return nil, nil }})
	c.Register(&Resource{Check: func(context.Context) error { return errors.New("busy terminal") }})
	stop, err := c.Bind(context.Background(), func(context.Context) error { t.Error("exec after failed check"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	waitAttempt(t, c)
	if prepared.Load() != 0 || c.Status().Error != "busy terminal" || c.Draining() {
		t.Fatal(c.Status(), prepared.Load())
	}
}

func TestRollbackDescendantsRetainOwnership(t *testing.T) {
	c := &Coordinator{Timeout: time.Second}
	recoveryStarted := make(chan struct{}, 2)
	recoveryFinish := make(chan struct{})
	defer close(recoveryFinish)
	unregister := c.Register(&Resource{Prepare: func(context.Context) (func(context.Context), error) {
		return func(ctx context.Context) {
			if err := c.Go(ctx, func(context.Context) { recoveryStarted <- struct{}{}; <-recoveryFinish }); err != nil {
				t.Error(err)
			}
		}, nil
	}})
	var executions atomic.Int32
	stop, err := c.Bind(context.Background(), func(context.Context) error { executions.Add(1); return errors.New("exec failed") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	waitAttempt(t, c)
	<-recoveryStarted
	unregister()
	c.Timeout = 20 * time.Millisecond
	c.Request()
	waitAttempt(t, c)
	if executions.Load() != 1 {
		t.Fatal("exec overtook rollback recovery")
	}
}

func TestAdmissionCancellationDoesNotCancelAdmittedWork(t *testing.T) {
	c := &Coordinator{Timeout: time.Second}
	work, release, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	intake, cancel := c.AdmissionContext(context.Background())
	defer cancel()
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("failed exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	select {
	case <-intake.Done():
	case <-time.After(time.Second):
		t.Fatal("idle intake not stopped")
	}
	if work.Err() != nil {
		t.Fatal("admitted operation cancelled")
	}
	release()
	waitAttempt(t, c)
	next, cancelNext := c.AdmissionContext(context.Background())
	defer cancelNext()
	if next.Err() != nil {
		t.Fatal("intake did not reopen")
	}
}

// Observe the wait loop without adding a production scheduling/test hook.
type resumeProbeContext struct {
	context.Context
	checked chan struct{}
}

func (ctx resumeProbeContext) Err() error {
	select {
	case ctx.checked <- struct{}{}:
	default:
	}
	return ctx.Context.Err()
}

func TestResumeWaitsAcrossReloadGenerationsAndOwnsFreshWork(t *testing.T) {
	c := &Coordinator{}
	old, release, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	old, closeTask := c.NewTask(old)
	defer closeTask()
	ctx, cancel := context.WithTimeout(old, time.Second)
	defer cancel()
	checked := make(chan struct{}, 4)
	first, second := make(chan struct{}), make(chan struct{})
	c.mu.Lock()
	c.draining = true
	c.done = first
	c.mu.Unlock()
	type result struct {
		ctx     context.Context
		release func()
		err     error
	}
	resumed := make(chan result, 1)
	go func() {
		ctx, release, err := c.Resume(resumeProbeContext{ctx, checked})
		resumed <- result{ctx, release, err}
	}()
	<-checked // Waiting on the first failed attempt.
	c.mu.Lock()
	close(first)
	c.done = second // Another attempt wins before the parked owner wakes up.
	c.mu.Unlock()
	<-checked
	select {
	case got := <-resumed:
		t.Fatalf("escaped second drain: %+v", got)
	default:
	}
	c.mu.Lock()
	c.draining = false
	close(second)
	c.mu.Unlock()
	got := <-resumed
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.release()
	if CurrentTask(got.ctx) != CurrentTask(old) {
		t.Fatal("lost original continuation task")
	}
	if _, _, err := Child(old); !errors.Is(err, ErrReleased) {
		t.Fatal("old ownership revived", err)
	}
	_, childDone, err := Child(got.ctx)
	if err != nil {
		t.Fatal("new ownership unusable", err)
	}
	childDone()
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active != 1 {
		t.Fatal("resume did not retain exactly one ticket", active)
	}
}

func TestResumeCancellationDoesNotAcquireOwnership(t *testing.T) {
	for _, draining := range []bool{false, true} {
		c := &Coordinator{draining: draining, done: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, release, err := c.Resume(ctx); !errors.Is(err, context.Canceled) || release != nil {
			t.Fatalf("cancelled resume admitted: %v", err)
		}
		if c.active != 0 {
			t.Fatal("cancelled resume retained ownership")
		}
	}
}
