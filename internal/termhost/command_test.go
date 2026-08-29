package termhost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
)

func TestCommandSinkAppliesConfiguredContextTimeout(t *testing.T) {
	cancelled := make(chan struct{})
	sink, err := newCommandSink(config.LifecycleCommandConfig{
		Name: "slow", Command: []string{"bridge"}, Timeout: "10ms",
	}, func(ctx context.Context, _ string, _ []string, _ []byte) error {
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = sink.Send(context.Background(), lifecycle.NewEvent(lifecycle.KindState, 1, time.Now(), lifecycle.Metadata{}, lifecycle.Snapshot{State: lifecycle.Working}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("configured timeout took %v", elapsed)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("runner context was not cancelled")
	}
}
