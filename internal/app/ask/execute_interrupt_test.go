package ask

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/term-llm/term-llm/internal/run"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/ui"
)

type interruptionRunner struct {
	started   chan struct{}
	canceled  chan struct{}
	returnErr error
}

func (r *interruptionRunner) Run(ctx context.Context, _ run.Request, _ run.EventSink) (run.Result, error) {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	if r.returnErr != nil {
		return run.Result{}, r.returnErr
	}
	return run.Result{}, ctx.Err()
}

func newInterruptionExecution(r *interruptionRunner, render func(context.Context, <-chan ui.StreamEvent) error) (*askStreamingExecution, *ui.StreamAdapter) {
	adapter := ui.NewStreamAdapter(4)
	return &askStreamingExecution{
		ctx:             context.Background(),
		stderr:          io.Discard,
		stdout:          io.Discard,
		cleanupTimeout:  time.Second,
		adapter:         adapter,
		runner:          r,
		applyResult:     func(run.Result) {},
		events:          adapter.Events(),
		useRichRenderer: true,
		renderFn:        render,
	}, adapter
}

func TestExecutionRendererInterruptionStopsOutstandingRunWithLiveContext(t *testing.T) {
	runner := &interruptionRunner{started: make(chan struct{}), canceled: make(chan struct{})}
	exec, _ := newInterruptionExecution(runner, func(context.Context, <-chan ui.StreamEvent) error {
		<-runner.started
		return errUserInterrupted
	})
	store := &askMemoryMessageStore{}
	exec.store = store
	exec.session = &session.Session{ID: "renderer-interrupt"}

	result, err := exec.run()
	if err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if !result.interrupted {
		t.Fatal("result.interrupted = false, want explicit renderer interruption")
	}
	select {
	case <-runner.canceled:
	default:
		t.Fatal("outstanding runner context was not canceled")
	}
	if exec.ctx.Err() != nil {
		t.Fatalf("outer context was canceled: %v", exec.ctx.Err())
	}
	if store.status != session.StatusInterrupted {
		t.Fatalf("persisted status = %q, want interrupted", store.status)
	}
}

func TestExecutionRendererInterruptionDoesNotHideProviderFailure(t *testing.T) {
	providerErr := errors.New("provider failed")
	runner := &interruptionRunner{started: make(chan struct{}), canceled: make(chan struct{}), returnErr: providerErr}
	exec, _ := newInterruptionExecution(runner, func(context.Context, <-chan ui.StreamEvent) error {
		<-runner.started
		return errUserInterrupted
	})
	store := &askMemoryMessageStore{}
	exec.store = store
	exec.session = &session.Session{ID: "renderer-interrupt"}

	result, err := exec.run()
	if err == nil || !strings.Contains(err.Error(), "streaming failed: provider failed") {
		t.Fatalf("run() error = %v, want provider failure", err)
	}
	if result.interrupted {
		t.Fatal("result.interrupted = true, want provider failure to remain authoritative")
	}
}

func TestRunStreamProgramDistinguishesUserAndContextCancellation(t *testing.T) {
	t.Run("user key", func(t *testing.T) {
		program := tea.NewProgram(newAskStreamModel(), tea.WithoutSignalHandler(), tea.WithInput(nil), tea.WithOutput(io.Discard))
		done := make(chan error, 1)
		go func() { done <- RunStreamProgram(context.Background(), program) }()
		time.Sleep(20 * time.Millisecond)
		program.Send(askUserCancelledMsg{})
		select {
		case err := <-done:
			if !errors.Is(err, errUserInterrupted) {
				t.Fatalf("RunStreamProgram error = %v, want errUserInterrupted", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("RunStreamProgram did not exit after user cancellation")
		}
	})

	t.Run("outer context", func(t *testing.T) {
		program := tea.NewProgram(newAskStreamModel(), tea.WithoutSignalHandler(), tea.WithInput(nil), tea.WithOutput(io.Discard))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- RunStreamProgram(ctx, program) }()
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) || errors.Is(err, errUserInterrupted) {
				t.Fatalf("RunStreamProgram error = %v, want plain context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("RunStreamProgram did not exit after context cancellation")
		}
	})
}

func TestExecutionRendererInterruptionDoesNotHideJoinedProviderFailure(t *testing.T) {
	providerErr := errors.New("provider failed")
	runner := &interruptionRunner{
		started:   make(chan struct{}),
		canceled:  make(chan struct{}),
		returnErr: errors.Join(providerErr, context.Canceled),
	}
	exec, _ := newInterruptionExecution(runner, func(context.Context, <-chan ui.StreamEvent) error {
		<-runner.started
		return errUserInterrupted
	})
	store := &askMemoryMessageStore{}
	exec.store = store
	exec.session = &session.Session{ID: "renderer-interrupt-joined"}

	result, err := exec.run()
	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("run() error = %v, want joined provider failure", err)
	}
	if result.interrupted {
		t.Fatal("result.interrupted = true, want provider failure to remain authoritative")
	}
}
