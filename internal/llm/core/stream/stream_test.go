package stream

import (
	"context"
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// A cancelled stream context must not turn a failed run into a clean EOF. The
// consumer reports cancellation only while the run goroutine is still live, so a
// dropped error made a failed run look successful whenever the goroutine
// finished first.
func TestNewEventStreamKeepsRunErrorWhenStreamContextIsCancelled(t *testing.T) {
	wantErr := errors.New("provider failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	st := New(ctx, func(ctx context.Context, send Sender) error {
		return wantErr
	})

	// Recv after the run goroutine has closed its channels, which is the ordering
	// that previously reported io.EOF.
	<-st.(*channelStream).done

	event, err := st.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v, want nil with a terminal error event", err)
	}
	if event.Type != core.EventError {
		t.Fatalf("Recv() event type = %v, want %v", event.Type, core.EventError)
	}
	if !errors.Is(event.Err, wantErr) {
		t.Fatalf("Recv() event error = %v, want %v", event.Err, wantErr)
	}
}
