package llm

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

type wrappedEOFStream struct{}

func (wrappedEOFStream) Recv() (Event, error) {
	return Event{}, fmt.Errorf("stream closed: %w", io.EOF)
}

func (wrappedEOFStream) Close() error { return nil }

func TestCollectTextStreamAcceptsWrappedEOF(t *testing.T) {
	result, err := CollectTextStream(wrappedEOFStream{}, nil)
	if err != nil {
		t.Fatalf("CollectTextStream() error = %v, want wrapped EOF treated as completion", err)
	}
	if result != (TextStreamResult{}) {
		t.Fatalf("result = %#v, want empty result", result)
	}
}

func TestCollectTextStreamNilErrorEventIsFatal(t *testing.T) {
	stream := &sliceStream{events: []Event{
		{Type: EventTextDelta, Text: "partial"},
		{Type: EventError},
	}}

	result, err := CollectTextStream(stream, nil)
	if err == nil || err.Error() != "provider stream reported an error" {
		t.Fatalf("CollectTextStream() error = %v, want terminal provider error", err)
	}
	if result.Text != "partial" {
		t.Fatalf("text = %q, want partial", result.Text)
	}
}

func TestCollectTextStreamResetsDiscardedAttempt(t *testing.T) {
	stream := &sliceStream{events: []Event{
		{Type: EventTextDelta, Text: "discard me"},
		{Type: EventUsage, Use: &Usage{InputTokens: 100, OutputTokens: 20}},
		{Type: EventAttemptDiscard},
		{Type: EventReasoningDelta, Text: "final reasoning", ReasoningKind: ReasoningKindSummary},
		{Type: EventTextDelta, Text: "final text"},
		{Type: EventUsage, Use: &Usage{InputTokens: 40, OutputTokens: 8}},
	}}

	result, err := CollectTextStream(stream, nil)
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
	stream := &sliceStream{events: []Event{{Type: EventError, Err: wantErr}}}
	observed := false

	_, err := CollectTextStream(stream, func(event Event) error {
		observed = event.Type == EventError
		return nil
	})
	if !errors.Is(err, wantErr) || !observed {
		t.Fatalf("CollectTextStream() = error %v observed %v, want provider error observed", err, observed)
	}
}

func TestCollectTextStreamObserverCanStopCollection(t *testing.T) {
	stopErr := errors.New("stop")
	stream := &sliceStream{events: []Event{
		{Type: EventTextDelta, Text: "before"},
		{Type: EventToolCall, Tool: &ToolCall{Name: "shell"}},
		{Type: EventTextDelta, Text: "after"},
	}}

	result, err := CollectTextStream(stream, func(event Event) error {
		if event.Type == EventToolCall {
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
