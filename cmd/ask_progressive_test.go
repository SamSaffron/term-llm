package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestAskProgressiveBridge_PropagatesInterjectionID(t *testing.T) {
	bridge := newAskProgressiveBridge(ui.DefaultStreamBufferSize)

	if err := bridge.HandleEvent(llm.Event{
		Type:           llm.EventInterjection,
		Text:           "keep sleeping",
		InterjectionID: "bridge-interject-1",
	}); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}

	ev := <-bridge.Events()
	if ev.Type != ui.StreamEventInterjection {
		t.Fatalf("event type = %v, want %v", ev.Type, ui.StreamEventInterjection)
	}
	if ev.Text != "keep sleeping" {
		t.Fatalf("event text = %q, want %q", ev.Text, "keep sleeping")
	}
	if ev.InterjectionID != "bridge-interject-1" {
		t.Fatalf("event interjection ID = %q, want %q", ev.InterjectionID, "bridge-interject-1")
	}
}

type failingJSONWriter struct{}

func (failingJSONWriter) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("broken pipe")
}

func TestAskProgressiveBridge_StopUnblocksProducerAfterConsumerWriteError(t *testing.T) {
	bridge := newAskProgressiveBridge(1)
	runCh := make(chan error, 1)

	go func() {
		for i := 0; i < 8; i++ {
			err := bridge.HandleEvent(llm.Event{Type: llm.EventTextDelta, Text: "chunk"})
			if err != nil {
				bridge.CloseError(err)
				runCh <- err
				return
			}
		}
		bridge.CloseSuccess()
		runCh <- nil
	}()

	_, _, writeErr := streamJSONEvents(context.Background(), bridge.Events(), newJSONEmitter(failingJSONWriter{}))
	if writeErr == nil {
		t.Fatal("expected streamJSONEvents to fail")
	}

	bridge.Stop()

	select {
	case err := <-runCh:
		if !errors.Is(err, errAskProgressiveBridgeStopped) {
			t.Fatalf("producer error = %v, want %v", err, errAskProgressiveBridgeStopped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer remained blocked after bridge.Stop")
	}

	select {
	case _, ok := <-bridge.Events():
		if ok {
			for range bridge.Events() {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge events channel was not closed")
	}
}
