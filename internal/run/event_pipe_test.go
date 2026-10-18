package run

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestEventPipeRecvAfterCloseIsStable(t *testing.T) {
	pipe := NewEventPipe(context.Background(), 1)
	pipe.Event(core.Event{Type: core.EventTextDelta, Text: "hello"})
	pipe.CloseWithError(nil)

	ev, err := pipe.Recv()
	if err != nil {
		t.Fatalf("first Recv err = %v", err)
	}
	if ev.Text != "hello" {
		t.Fatalf("first Recv text = %q, want hello", ev.Text)
	}
	for i := 0; i < 2; i++ {
		_, err = pipe.Recv()
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Recv after EOF #%d err = %v, want EOF", i+1, err)
		}
	}
}

func TestEventPipeRecvAfterErrorIsStable(t *testing.T) {
	wantErr := errors.New("boom")
	pipe := NewEventPipe(context.Background(), 0)
	pipe.CloseWithError(wantErr)

	for i := 0; i < 2; i++ {
		_, err := pipe.Recv()
		if !errors.Is(err, wantErr) {
			t.Fatalf("Recv after error #%d err = %v, want %v", i+1, err, wantErr)
		}
	}
}
