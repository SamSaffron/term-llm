package restart

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGraceCancelsOnlyOptedInStepsAndStillJoinsOwnership(t *testing.T) {
	c := &Coordinator{InterruptAfter: 10 * time.Millisecond}
	ctx, release, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	taskCtx, closeTask := c.NewTask(ctx)
	defer closeTask()
	step, finish := CurrentTask(taskCtx).Step(taskCtx)
	defer finish()
	replaced := make(chan struct{}, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error { replaced <- struct{}{}; return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	select {
	case <-step.Done():
	case <-time.After(time.Second):
		t.Fatal("grace did not request cancellation")
	}
	if !errors.Is(context.Cause(step), ErrInterrupt) || ctx.Err() != nil {
		t.Fatal("cancelled invocation instead of its step", context.Cause(step), ctx.Err())
	}
	select {
	case <-replaced:
		t.Fatal("cancelled step was mistaken for settled execution")
	default:
	}
	finish()
	release()
	waitAttempt(t, c)
	if !errors.Is(context.Cause(step), ErrInterrupt) || c.Status().Phase != "ready" {
		t.Fatal(c.Status())
	}
	next, done := CurrentTask(taskCtx).Step(context.Background())
	defer done()
	if next.Err() != nil {
		t.Fatal("failed exec left future steps cancelled")
	}
}

func TestCancellationOnlyTaskDoesNotAdvertiseContinuation(t *testing.T) {
	c := &Coordinator{InterruptAfter: time.Millisecond}
	ctx, release, _ := c.Root(context.Background())
	defer release()
	ctx, finish := c.Cancellable(ctx)
	defer finish()
	if CurrentTask(ctx) != nil {
		t.Fatal("ordinary operation advertised a resumable execution")
	}
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ordinary operation not cancelled")
	}
}

func TestTaskClaimsOneEngineAndNotifiesPendingBoundary(t *testing.T) {
	c := &Coordinator{}
	ctx, release, _ := c.Root(context.Background())
	defer release()
	ctx, closeTask := c.NewTask(ctx)
	defer closeTask()
	task := CurrentTask(ctx)
	if !task.Claim("parent") || task.Claim("nested helper") {
		t.Fatal("nested helper took continuation ownership")
	}
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	notified := make(chan struct{}, 2)
	task.OnRequest(func() { notified <- struct{}{} })
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("late provider did not see pending restart")
	}
	task.SetSuspendAllowed(func() bool { return false })
	if task.Pending() {
		t.Fatal("uncapturable run allowed to suspend")
	}
	task.Release("parent")
	if !task.Claim("next engine") {
		t.Fatal("next engine could not own subsequent execution")
	}
}
