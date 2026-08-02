package cmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/serve"
)

type transientFailurePlatform struct {
	runCalls  atomic.Int32
	connected chan struct{}
}

func (p *transientFailurePlatform) Name() string     { return "test" }
func (p *transientFailurePlatform) NeedsSetup() bool { return false }
func (p *transientFailurePlatform) RunSetup() error  { return nil }

func (p *transientFailurePlatform) Run(ctx context.Context, _ *config.Config, _ serve.Settings) error {
	switch p.runCalls.Add(1) {
	case 1:
		return errors.New("transient startup failure")
	case 2:
		close(p.connected)
		<-ctx.Done()
		return ctx.Err()
	default:
		return errors.New("unexpected extra run")
	}
}

func TestRunPlatformSupervisorRetriesAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	platform := &transientFailurePlatform{connected: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		runPlatformSupervisor(ctx, &config.Config{}, serve.Settings{}, platform, time.Millisecond)
		close(done)
	}()

	select {
	case <-platform.connected:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("platform was not restarted after transient failure")
	}
	if calls := platform.runCalls.Load(); calls != 2 {
		cancel()
		t.Fatalf("Run calls = %d, want 2", calls)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop promptly after cancellation")
	}
	if calls := platform.runCalls.Load(); calls != 2 {
		t.Fatalf("Run calls after cancellation = %d, want 2", calls)
	}
}
