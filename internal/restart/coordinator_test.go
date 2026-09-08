package restart

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitAttempt(t *testing.T, c *Coordinator) {
	t.Helper()
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("attempt did not finish")
	}
}

func TestDrainOwnsChildrenAfterParentCancellation(t *testing.T) {
	c := &Coordinator{Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	ctx, release, err := c.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error { called <- struct{}{}; return errors.New("exec failed") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	if _, _, err := c.Enter(context.Background()); !errors.Is(err, ErrDraining) {
		t.Fatal(err)
	}
	// An already-admitted operation may still spawn children while draining.
	_, childDone, err := c.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	release()
	release()
	select {
	case <-called:
		t.Fatal("cancelled caller was mistaken for completed tool")
	default:
	}
	if _, _, err := c.Enter(ctx); !errors.Is(err, ErrReleased) {
		t.Fatal(err)
	}
	childDone()
	waitAttempt(t, c)
	if c.Draining() || c.Status().Phase != "ready" || c.Status().Error != "exec failed" {
		t.Fatal(c.Status())
	}
	_, done, err := c.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done()
}

func TestTimeoutDoesNotCancelWorkAndCanRetry(t *testing.T) {
	c := &Coordinator{Timeout: 10 * time.Millisecond}
	ctx, done, _ := c.Enter(context.Background())
	var calls atomic.Int32
	stop, err := c.Bind(context.Background(), func(context.Context) error { calls.Add(1); return errors.New("test exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	c.Request()
	waitAttempt(t, c)
	if calls.Load() != 0 || ctx.Err() != nil || c.Draining() || c.Status().Attempt != 1 || c.Status().Error == "" {
		t.Fatal(c.Status())
	}
	done()
	c.Request()
	waitAttempt(t, c)
	if calls.Load() != 1 || c.Status().Attempt != 2 {
		t.Fatal(c.Status())
	}
}

func TestStartupSignalCoalescesUntilReady(t *testing.T) {
	c := &Coordinator{}
	c.Request()
	c.Request()
	if c.Status().Phase != "deferred" {
		t.Fatal(c.Status())
	}
	var calls atomic.Int32
	stop, err := c.Bind(context.Background(), func(context.Context) error { calls.Add(1); return errors.New("test exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	waitAttempt(t, c)
	if calls.Load() != 1 || c.Status().Attempt != 1 {
		t.Fatal(c.Status())
	}
	if _, err := c.Bind(context.Background(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("accepted competing owner")
	}
}

func TestUnbindJoinsAndShutdownPreventsExec(t *testing.T) {
	c := &Coordinator{}
	_, release, _ := c.Enter(context.Background())
	defer release()
	var calls atomic.Int32
	stop, err := c.Bind(context.Background(), func(context.Context) error { calls.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	c.Request()
	stop()
	stop()
	if calls.Load() != 0 || c.Draining() || c.Status().Phase != "deferred" {
		t.Fatal(c.Status())
	}
	// A fresh owner can bind after the previous attempt has joined.
	stop, err = c.Bind(context.Background(), func(context.Context) error { return errors.New("test") })
	if err != nil {
		t.Fatal(err)
	}
	stop()
}

func TestUnbindWaitsForReplacementCallback(t *testing.T) {
	c := &Coordinator{}
	entered, leave := make(chan struct{}), make(chan struct{})
	stop, err := c.Bind(context.Background(), func(ctx context.Context) error { close(entered); <-leave; return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	c.Request()
	<-entered
	joined := make(chan struct{})
	go func() { stop(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("unbind did not join callback")
	case <-time.After(10 * time.Millisecond):
	}
	close(leave)
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("unbind stuck")
	}
}

func TestDefaultRestartStaysPendingWithoutDeadline(t *testing.T) {
	c := &Coordinator{}
	work, release, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	deadlines := make(chan bool, 1)
	stop, err := c.Bind(context.Background(), func(ctx context.Context) error {
		_, limited := ctx.Deadline()
		deadlines <- limited
		return errors.New("fixture exec failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	c.Request()
	if c.Status().Phase != "draining" || c.Status().Attempt != 1 || work.Err() != nil {
		t.Fatal("restart did not remain pending with admitted work", c.Status())
	}
	select {
	case <-deadlines:
		t.Fatal("exec overtook admitted work")
	default:
	}
	release()
	waitAttempt(t, c)
	if <-deadlines {
		t.Fatal("default restart still has an automatic deadline")
	}
}
