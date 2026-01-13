package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

type sliceStream struct {
	events []Event
	index  int
}

func (s *sliceStream) Recv() (Event, error) {
	if s.index >= len(s.events) {
		return Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *sliceStream) Close() error {
	return nil
}

type fakeProvider struct {
	script func(call int, req Request) []Event
	calls  []Request
}

func (p *fakeProvider) Name() string {
	return "fake"
}

func (p *fakeProvider) Credential() string {
	return "test"
}

func (p *fakeProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch: false,
		NativeWebFetch:  false,
		ToolCalls:       true,
	}
}

func (p *fakeProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.calls = append(p.calls, req)
	call := len(p.calls) - 1
	events := p.script(call, req)
	return &sliceStream{events: events}, nil
}

type countingSearchTool struct {
	calls int
}

func (t *countingSearchTool) Spec() ToolSpec {
	return WebSearchToolSpec()
}

func (t *countingSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	t.calls++
	return fmt.Sprintf("result %d", t.calls), nil
}

func (t *countingSearchTool) Preview(args json.RawMessage) string {
	return ""
}

func TestEngineExternalSearchLoopsUntilNoToolCalls(t *testing.T) {
	tool := &countingSearchTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: WebSearchToolName, Arguments: json.RawMessage(`{"query":"zig"}`)}},
					{Type: EventDone},
				}
			case 1:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-2", Name: WebSearchToolName, Arguments: json.RawMessage(`{"query":"zig release"}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{
					{Type: EventTextDelta, Text: "final answer"},
					{Type: EventDone},
				}
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("latest release")},
		Search:   true,
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var toolEvents int
	var gotErr error

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		switch event.Type {
		case EventError:
			gotErr = event.Err
		case EventToolCall:
			toolEvents++
		case EventTextDelta:
			text.WriteString(event.Text)
		}
	}

	if gotErr != nil {
		t.Fatalf("unexpected stream error: %v", gotErr)
	}
	if toolEvents != 0 {
		t.Fatalf("unexpected tool call events: %d", toolEvents)
	}
	if text.String() != "final answer" {
		t.Fatalf("unexpected text: %q", text.String())
	}
	if tool.calls != 2 {
		t.Fatalf("expected 2 tool calls, got %d", tool.calls)
	}
	if len(provider.calls) != 3 {
		t.Fatalf("expected 3 provider calls, got %d", len(provider.calls))
	}

	last := provider.calls[len(provider.calls)-1]
	if countToolResults(last.Messages) != 2 {
		t.Fatalf("expected 2 tool results in final request")
	}
	if countToolCalls(last.Messages) != 2 {
		t.Fatalf("expected 2 tool calls in final request")
	}
}

func TestEngineExternalSearchStopsAfterMaxLoops(t *testing.T) {
	tool := &countingSearchTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventToolCall, Tool: &ToolCall{ID: fmt.Sprintf("call-%d", call), Name: WebSearchToolName, Arguments: json.RawMessage(`{"query":"zig"}`)}},
				{Type: EventDone},
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("latest release")},
		Search:   true,
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var gotErr error
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventError && event.Err != nil {
			gotErr = event.Err
		}
	}

	if gotErr == nil || !strings.Contains(gotErr.Error(), "external tools exceeded max turns") {
		t.Fatalf("expected max turns error, got %v", gotErr)
	}

	expectedCalls := 1 + defaultMaxTurns
	if len(provider.calls) != expectedCalls {
		t.Fatalf("expected %d provider calls, got %d", expectedCalls, len(provider.calls))
	}

	last := provider.calls[len(provider.calls)-1]
	if !hasSystemText(last.Messages, stopSearchToolHint) {
		t.Fatalf("expected stop hint in final request")
	}
}

func countToolResults(messages []Message) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == PartToolResult {
				count++
			}
		}
	}
	return count
}

func countToolCalls(messages []Message) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == PartToolCall {
				count++
			}
		}
	}
	return count
}

func hasSystemText(messages []Message, text string) bool {
	for _, msg := range messages {
		if msg.Role != RoleSystem {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type == PartText && strings.Contains(part.Text, text) {
				return true
			}
		}
	}
	return false
}
