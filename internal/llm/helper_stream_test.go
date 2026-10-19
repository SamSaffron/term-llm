package llm

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

type wrappedEOFStream struct{}

func (wrappedEOFStream) Recv() (core.Event, error) {
	return core.Event{}, fmt.Errorf("stream closed: %w", io.EOF)
}

func (wrappedEOFStream) Close() error { return nil }

func TestCollectTextStreamAcceptsWrappedEOF(t *testing.T) {
	result, err := core.CollectTextStream(wrappedEOFStream{}, nil)
	if err != nil {
		t.Fatalf("CollectTextStream() error = %v, want wrapped EOF treated as completion", err)
	}
	if result != (core.TextStreamResult{}) {
		t.Fatalf("result = %#v, want empty result", result)
	}
}

func TestCollectTextStreamNilErrorEventIsFatal(t *testing.T) {
	stream := &sliceStream{events: []core.Event{
		{Type: core.EventTextDelta, Text: "partial"},
		{Type: core.EventError},
	}}

	result, err := core.CollectTextStream(stream, nil)
	if err == nil || err.Error() != "provider stream reported an error" {
		t.Fatalf("CollectTextStream() error = %v, want terminal provider error", err)
	}
	if result.Text != "partial" {
		t.Fatalf("text = %q, want partial", result.Text)
	}
}

func TestCollectTextStreamResetsDiscardedAttempt(t *testing.T) {
	stream := &sliceStream{events: []core.Event{
		{Type: core.EventTextDelta, Text: "discard me"},
		{Type: core.EventUsage, Use: &core.Usage{InputTokens: 100, OutputTokens: 20}},
		{Type: core.EventAttemptDiscard},
		{Type: core.EventReasoningDelta, Text: "final reasoning", ReasoningKind: core.ReasoningKindSummary},
		{Type: core.EventTextDelta, Text: "final text"},
		{Type: core.EventUsage, Use: &core.Usage{InputTokens: 40, OutputTokens: 8}},
	}}

	result, err := core.CollectTextStream(stream, nil)
	if err != nil {
		t.Fatalf("CollectTextStream() error = %v", err)
	}
	if result.Text != "final text" || result.ReasoningSummary != "final reasoning" {
		t.Fatalf("result = %#v, want final attempt only", result)
	}
	if result.Usage.InputTokens != 40 || result.Usage.OutputTokens != 8 {
		t.Fatalf("usage = %+v, want final attempt only", result.Usage)
	}
}

func TestCollectTextStreamObserverSeesErrorEvent(t *testing.T) {
	wantErr := errors.New("provider failed")
	stream := &sliceStream{events: []core.Event{{Type: core.EventError, Err: wantErr}}}
	observed := false

	_, err := core.CollectTextStream(stream, func(event core.Event) error {
		observed = event.Type == core.EventError
		return nil
	})
	if !errors.Is(err, wantErr) || !observed {
		t.Fatalf("CollectTextStream() = error %v observed %v, want provider error observed", err, observed)
	}
}

func TestCollectTextStreamObserverCanStopCollection(t *testing.T) {
	stopErr := errors.New("stop")
	stream := &sliceStream{events: []core.Event{
		{Type: core.EventTextDelta, Text: "before"},
		{Type: core.EventToolCall, Tool: &core.ToolCall{Name: "shell"}},
		{Type: core.EventTextDelta, Text: "after"},
	}}

	result, err := core.CollectTextStream(stream, func(event core.Event) error {
		if event.Type == core.EventToolCall {
			return stopErr
		}
		return nil
	})
	if !errors.Is(err, stopErr) {
		t.Fatalf("CollectTextStream() error = %v, want stop", err)
	}
	if result.Text != "before" {
		t.Fatalf("text = %q, want content before observer stop", result.Text)
	}
}
