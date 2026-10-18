package ui

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
)

type multiTurnIDProvider struct {
	requests []core.Request
}

func (p *multiTurnIDProvider) Name() string       { return "multi-turn-id-test" }
func (p *multiTurnIDProvider) Credential() string { return "test" }
func (p *multiTurnIDProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true}
}

func (p *multiTurnIDProvider) Stream(_ context.Context, req core.Request) (core.Stream, error) {
	turn := len(p.requests)
	p.requests = append(p.requests, req)

	var events []core.Event
	switch turn {
	case 0, 1:
		events = []core.Event{
			{Type: core.EventToolCall, Tool: &core.ToolCall{Name: "first_tool", Arguments: json.RawMessage(`{"turn":1}`)}},
			{Type: core.EventToolCall, Tool: &core.ToolCall{Name: "second_tool", Arguments: json.RawMessage(`{"turn":2}`)}},
			{Type: core.EventDone},
		}
	default:
		events = []core.Event{
			{Type: core.EventTextDelta, Text: "reply one. "},
			{Type: core.EventTextDelta, Text: "reply two."},
			{Type: core.EventDone},
		}
	}
	return &testStream{events: events}, nil
}

type multiTurnIDTool struct {
	name string
}

func (t multiTurnIDTool) Spec() core.ToolSpec { return core.ToolSpec{Name: t.name} }
func (t multiTurnIDTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("ok"), nil
}
func (t multiTurnIDTool) Preview(json.RawMessage) string { return t.name }

func TestStreamAdapterPreservesRepeatedAutomaticToolTurns(t *testing.T) {
	provider := &multiTurnIDProvider{}
	registry := engine.NewToolRegistry()
	first := multiTurnIDTool{name: "first_tool"}
	second := multiTurnIDTool{name: "second_tool"}
	registry.Register(first)
	registry.Register(second)
	eng := engine.NewEngine(provider, registry)

	stream, err := eng.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("run two tool turns, then reply")},
		Tools:    []core.ToolSpec{first.Spec(), second.Spec()},
		MaxTurns: 5,
	})
	if err != nil {
		t.Fatalf("Engine.Stream: %v", err)
	}
	defer stream.Close()

	adapter := NewStreamAdapter(32)
	go adapter.ProcessStream(context.Background(), stream)

	startIDs := make(map[string]struct{})
	endIDs := make(map[string]struct{})
	tracker := NewToolTracker()
	var starts, ends int
	var text string
	for event := range adapter.Events() {
		switch event.Type {
		case StreamEventToolStart:
			starts++
			if event.ToolCallID == "" {
				t.Fatal("tool start had an empty ID")
			}
			if _, exists := startIDs[event.ToolCallID]; exists {
				t.Fatalf("tool start ID was reused across automatic turns: %q", event.ToolCallID)
			}
			startIDs[event.ToolCallID] = struct{}{}
			if !tracker.HandleToolStart(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs) {
				t.Fatalf("tool tracker rejected distinct call %q", event.ToolCallID)
			}
		case StreamEventToolEnd:
			ends++
			if event.ToolCallID == "" {
				t.Fatal("tool end had an empty ID")
			}
			endIDs[event.ToolCallID] = struct{}{}
			tracker.HandleToolEnd(event.ToolCallID, event.ToolSuccess)
		case StreamEventText:
			text += event.Text
		}
	}

	if starts != 4 || ends != 4 {
		t.Fatalf("tool events: starts=%d ends=%d, want 4 each", starts, ends)
	}
	if len(tracker.Segments) != 4 || tracker.HasPending() {
		t.Fatalf("tool tracker segments=%d pending=%v, want four completed rows", len(tracker.Segments), tracker.HasPending())
	}
	for id := range startIDs {
		if _, ok := endIDs[id]; !ok {
			t.Errorf("tool start %q had no matching end", id)
		}
	}
	if text != "reply one. reply two." {
		t.Fatalf("reply text = %q", text)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("provider turns = %d, want 3", len(provider.requests))
	}

	persistedIDs := make(map[string]struct{})
	for _, message := range provider.requests[2].Messages {
		for _, part := range message.Parts {
			if part.Type != core.PartToolCall || part.ToolCall == nil {
				continue
			}
			if _, exists := persistedIDs[part.ToolCall.ID]; exists {
				t.Fatalf("persisted tool-call ID was reused: %q", part.ToolCall.ID)
			}
			persistedIDs[part.ToolCall.ID] = struct{}{}
		}
	}
	if len(persistedIDs) != 4 {
		t.Fatalf("persisted tool-call IDs = %d, want 4", len(persistedIDs))
	}

	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("stream after adapter completion: %v, want EOF", err)
	}
}
