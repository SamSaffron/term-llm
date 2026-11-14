package ask

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/run"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/ui"
)

type progressiveBlockingRunner struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	ignore   bool
	err      error
}

func (r *progressiveBlockingRunner) Run(ctx context.Context, _ run.Request, _ run.EventSink) (run.Result, error) {
	close(r.started)
	if r.ignore {
		<-r.release
		return run.Result{}, context.Canceled
	}
	<-ctx.Done()
	close(r.canceled)
	if r.err != nil {
		return run.Result{}, r.err
	}
	return run.Result{}, ctx.Err()
}

func newProgressiveInterruptionExecution(r *progressiveBlockingRunner, render func(context.Context, <-chan ui.StreamEvent) error) *askProgressiveExecution {
	if r.release == nil {
		r.release = make(chan struct{})
	}
	return &askProgressiveExecution{
		ctx:             context.Background(),
		stderr:          io.Discard,
		stdout:          io.Discard,
		flags:           Options{},
		cfg:             &config.Config{},
		cleanupTimeout:  50 * time.Millisecond,
		runner:          r,
		applyResult:     func(run.Result) {},
		WrapEvents:      func(in <-chan ui.StreamEvent) <-chan ui.StreamEvent { return in },
		useRichRenderer: true,
		renderFn:        render,
	}
}

func TestProgressiveRendererInterruptionCancelsChildRun(t *testing.T) {
	runner := &progressiveBlockingRunner{started: make(chan struct{}), canceled: make(chan struct{})}
	exec := newProgressiveInterruptionExecution(runner, func(context.Context, <-chan ui.StreamEvent) error {
		<-runner.started
		return errUserInterrupted
	})
	store := &askMemoryMessageStore{}
	exec.store = store
	exec.session = &session.Session{ID: "progressive-interrupt"}

	result, err := exec.run()
	if err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if !result.interrupted {
		t.Fatal("result.interrupted = false")
	}
	select {
	case <-runner.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("progressive runner context was not canceled")
	}
	if exec.ctx.Err() != nil {
		t.Fatalf("outer context was canceled: %v", exec.ctx.Err())
	}
	if store.status != session.StatusInterrupted {
		t.Fatalf("persisted status = %q", store.status)
	}
}

func TestProgressiveRendererInterruptionDoesNotHideProviderFailure(t *testing.T) {
	providerErr := errors.New("provider failed")
	runner := &progressiveBlockingRunner{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		err:      errors.Join(providerErr, context.Canceled),
	}
	exec := newProgressiveInterruptionExecution(runner, func(context.Context, <-chan ui.StreamEvent) error {
		<-runner.started
		return errUserInterrupted
	})

	result, err := exec.run()
	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("run() error = %v, want provider diagnostic", err)
	}
	if result.interrupted {
		t.Fatal("provider failure was classified as interruption")
	}
}

func TestProgressiveBlockedRunnerIsBoundedAfterRendererExit(t *testing.T) {
	runner := &progressiveBlockingRunner{
		started: make(chan struct{}),
		release: make(chan struct{}),
		ignore:  true,
	}
	exec := newProgressiveInterruptionExecution(runner, func(context.Context, <-chan ui.StreamEvent) error {
		<-runner.started
		return errUserInterrupted
	})
	start := time.Now()
	result, err := exec.run()
	if err != nil || !result.interrupted {
		t.Fatalf("run() = (%+v, %v), want bounded interruption", result, err)
	}
	close(runner.release)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("blocked progressive runner cleanup took %s", elapsed)
	}
}
