package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestAskProgressiveRunnerSinkAccountsGuardianUsage(t *testing.T) {
	bridge := newAskProgressiveBridge(1)
	sink := askProgressiveRunnerSink{bridge: bridge}
	usage := llm.Usage{InputTokens: 13, OutputTokens: 4, CachedInputTokens: 8, CacheWriteTokens: 2}

	event := tools.GuardianEvent{ToolCallID: "call-1", Model: "guardian-model", Message: "guardian: approved", Usage: usage}
	sink.GuardianEvent(event)

	got := <-bridge.Events()
	if got.Type != ui.StreamEventGuardian || got.Guardian.ToolCallID != event.ToolCallID || got.Guardian.Message != event.Message {
		t.Fatalf("progressive guardian event = %+v", got)
	}
	calls, _ := bridge.Stats().UsageCalls()
	if bridge.stats.InputTokens != 13 || bridge.stats.OutputTokens != 4 || bridge.stats.CachedInputTokens != 8 || bridge.stats.CacheWriteTokens != 2 || len(calls) != 1 || !calls[0].Guardian || calls[0].Model != "guardian-model" {
		t.Fatalf("progressive guardian stats = %+v calls=%+v", bridge.stats, calls)
	}
}

func TestAskProgressiveBridge_PropagatesSteeringID(t *testing.T) {
	bridge := newAskProgressiveBridge(ui.DefaultStreamBufferSize)

	if err := bridge.HandleEvent(llm.Event{
		Type:       llm.EventSteering,
		Text:       "keep sleeping",
		SteeringID: "bridge-steer-1",
	}); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}

	ev := <-bridge.Events()
	if ev.Type != ui.StreamEventSteering {
		t.Fatalf("event type = %v, want %v", ev.Type, ui.StreamEventSteering)
	}
	if ev.Text != "keep sleeping" {
		t.Fatalf("event text = %q, want %q", ev.Text, "keep sleeping")
	}
	if ev.SteeringID != "bridge-steer-1" {
		t.Fatalf("event steering ID = %q, want %q", ev.SteeringID, "bridge-steer-1")
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
