package ui

import (
	"context"
	"io"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

type testStream struct {
	events []llm.Event
	index  int
}

func (s *testStream) Recv() (llm.Event, error) {
	if s.index >= len(s.events) {
		return llm.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *testStream) Close() error {
	return nil
}

func TestStreamAdapterDedupesToolEvents(t *testing.T) {
	stream := &testStream{
		events: []llm.Event{
			{Type: llm.EventToolExecStart, ToolCallID: "call-1", ToolName: "shell", ToolInfo: "(git status)"},
			{Type: llm.EventToolExecStart, ToolCallID: "call-1", ToolName: "shell", ToolInfo: "(git status)"},
			{Type: llm.EventToolExecEnd, ToolCallID: "call-1", ToolName: "shell", ToolInfo: "(git status)", ToolSuccess: true},
			{Type: llm.EventToolExecEnd, ToolCallID: "call-1", ToolName: "shell", ToolInfo: "(git status)", ToolSuccess: true},
		},
	}

	adapter := NewStreamAdapter(10)
	go adapter.ProcessStream(context.Background(), stream)

	var starts int
	var ends int
	for ev := range adapter.Events() {
		switch ev.Type {
		case StreamEventToolStart:
			starts++
		case StreamEventToolEnd:
			ends++
		}
	}

	if starts != 1 {
		t.Fatalf("expected 1 tool start event, got %d", starts)
	}
	if ends != 1 {
		t.Fatalf("expected 1 tool end event, got %d", ends)
	}
}

func TestStreamAdapterDedupesToolCallAndExecStart(t *testing.T) {
	// Simulate: EventToolCall (during streaming) followed by EventToolExecStart (at execution)
	// Both should result in only ONE ToolStartEvent
	stream := &testStream{
		events: []llm.Event{
			// During streaming, we receive EventToolCall
			{Type: llm.EventToolCall, ToolCallID: "call-1", ToolName: "read_file", Tool: &llm.ToolCall{
				ID:        "call-1",
				Name:      "read_file",
				Arguments: []byte(`{"file_path": "test.txt"}`),
			}},
			// Later, at execution, we receive EventToolExecStart
			{Type: llm.EventToolExecStart, ToolCallID: "call-1", ToolName: "read_file", ToolInfo: "test.txt"},
			// And the end event
			{Type: llm.EventToolExecEnd, ToolCallID: "call-1", ToolName: "read_file", ToolSuccess: true},
		},
	}

	adapter := NewStreamAdapter(10)
	go adapter.ProcessStream(context.Background(), stream)

	var starts int
	var ends int
	for ev := range adapter.Events() {
		switch ev.Type {
		case StreamEventToolStart:
			starts++
		case StreamEventToolEnd:
			ends++
		}
	}

	if starts != 1 {
		t.Fatalf("expected 1 tool start event (deduped EventToolCall + EventToolExecStart), got %d", starts)
	}
	if ends != 1 {
		t.Fatalf("expected 1 tool end event, got %d", ends)
	}
}
