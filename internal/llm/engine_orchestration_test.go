package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

type mockTool struct {
	name   string
	result string
	err    error
}

func (m *mockTool) Spec() ToolSpec {
	return ToolSpec{Name: m.name}
}

func (m *mockTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.result, nil
}

func (m *mockTool) Preview(args json.RawMessage) string {
	return ""
}

func TestEngineOrchestration_BasicToolLoop(t *testing.T) {
	ctx := context.Background()
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})

	provider := NewMockProvider("test")
	provider.AddToolCall("id-1", "test_tool", map[string]any{"arg": "val"})
	provider.AddTextResponse("final answer")

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("hello")},
		Tools:    []ToolSpec{{Name: "test_tool"}},
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var events []Event
	for {
		event, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				t.Logf("Recv error: %v", err)
			}
			break
		}
		t.Logf("Received event: %+v", event)
		events = append(events, event)
	}

	// Verify events
	hasToolExecStart := false
	hasToolExecEnd := false
	var fullText strings.Builder
	for _, e := range events {
		if e.Type == EventToolExecStart && e.ToolName == "test_tool" {
			hasToolExecStart = true
		}
		if e.Type == EventToolExecEnd && e.ToolName == "test_tool" && e.ToolSuccess {
			hasToolExecEnd = true
		}
		if e.Type == EventTextDelta {
			fullText.WriteString(e.Text)
		}
	}

	if !hasToolExecStart {
		t.Error("Missing EventToolExecStart")
	}
	if !hasToolExecEnd {
		t.Error("Missing EventToolExecEnd")
	}
	if !strings.Contains(fullText.String(), "final answer") {
		t.Errorf("Missing final answer text, got: %q", fullText.String())
	}

	// Verify provider calls
	if len(provider.Requests) != 2 {
		t.Errorf("Expected 2 provider calls, got %d", len(provider.Requests))
	}

	// Check second request contains tool results
	secondReq := provider.Requests[1]
	foundResult := false
	for _, msg := range secondReq.Messages {
		for _, part := range msg.Parts {
			if part.Type == PartToolResult && part.ToolResult.Name == "test_tool" && part.ToolResult.Content == "tool output" {
				foundResult = true
			}
		}
	}
	if !foundResult {
		t.Error("Tool result not found in second request")
	}
}

func TestEngineOrchestration_ExternalSearch(t *testing.T) {
	ctx := context.Background()
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: WebSearchToolName, result: "search results"})

	provider := NewMockProvider("test")
	provider.WithCapabilities(Capabilities{ToolCalls: true, NativeWebSearch: false})

	// Turn 0: LLM decides to search naturally during conversation
	provider.AddToolCall("search-1", WebSearchToolName, map[string]any{"query": "zig"})
	// Turn 1: LLM provides answer based on search results
	provider.AddTextResponse("zig is a language")

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("what is zig")},
		Search:   true,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
	}

	// Verify provider calls - should be exactly 2 (search + answer)
	if len(provider.Requests) != 2 {
		t.Errorf("Expected 2 provider calls, got %d", len(provider.Requests))
	}

	// Verify first request has web_search tool injected
	firstReq := provider.Requests[0]
	hasSearchTool := false
	for _, tool := range firstReq.Tools {
		if tool.Name == WebSearchToolName {
			hasSearchTool = true
			break
		}
	}
	if !hasSearchTool {
		t.Error("First request should have web_search tool injected")
	}
}

func TestEngineOrchestration_MaxTurns(t *testing.T) {
	ctx := context.Background()
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "loop_tool", result: "looping"})

	provider := NewMockProvider("test")
	// Add more than defaultMaxTurns (20) tool calls
	for i := 0; i < 25; i++ {
		provider.AddToolCall(fmt.Sprintf("id-%d", i), "loop_tool", nil)
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("loop")},
		Tools:    []ToolSpec{{Name: "loop_tool"}},
		MaxTurns: 3,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var gotErr error
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		if event.Type == EventError {
			gotErr = event.Err
		}
	}

	if gotErr == nil || !strings.Contains(gotErr.Error(), "exceeded max turns") {
		t.Errorf("Expected max turns error, got %v", gotErr)
	}

	if len(provider.Requests) != 3 {
		t.Errorf("Expected 3 provider calls (MaxTurns), got %d", len(provider.Requests))
	}
}

func TestEngineOrchestration_UnregisteredToolPassthrough(t *testing.T) {
	ctx := context.Background()
	registry := NewToolRegistry()

	provider := NewMockProvider("test")
	provider.AddToolCall("id-1", "unregistered_tool", map[string]any{"arg": "val"})

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("hello")},
		Tools:    []ToolSpec{{Name: "unregistered_tool"}},
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var toolCalls []ToolCall
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		if event.Type == EventToolCall {
			toolCalls = append(toolCalls, *event.Tool)
		}
	}

	if len(toolCalls) != 1 {
		t.Errorf("Expected 1 tool call event, got %d", len(toolCalls))
	} else if toolCalls[0].Name != "unregistered_tool" {
		t.Errorf("Expected unregistered_tool, got %s", toolCalls[0].Name)
	}
}

func TestEngineOrchestration_ExternalSearchMixedCalls(t *testing.T) {
	ctx := context.Background()
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: WebSearchToolName, result: "search results"})

	provider := NewMockProvider("test")
	provider.WithCapabilities(Capabilities{ToolCalls: true, NativeWebSearch: false})

	// Turn 0: LLM returns BOTH search AND an unregistered tool
	// This tests that search tools work alongside other tools naturally
	provider.AddTurn(MockTurn{
		ToolCalls: []ToolCall{
			{ID: "search-1", Name: WebSearchToolName, Arguments: json.RawMessage(`{"query":"zig"}`)},
			{ID: "unreg-1", Name: "suggest_something", Arguments: json.RawMessage(`{}`)},
		},
	})
	provider.AddTextResponse("done")

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("what is zig")},
		Search:   true,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var gotErr error
	var unregisteredToolCalls []ToolCall
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		if event.Type == EventError {
			gotErr = event.Err
		}
		if event.Type == EventToolCall {
			unregisteredToolCalls = append(unregisteredToolCalls, *event.Tool)
		}
	}

	if gotErr != nil {
		t.Errorf("Did not expect error for mixed calls, got: %v", gotErr)
	}

	// All tool calls are now passed through as events to preserve interleaving order
	// We expect 2 events: 1 registered (web_search) + 1 unregistered (suggest_something)
	if len(unregisteredToolCalls) != 2 {
		t.Errorf("Expected 2 tool call events, got %d", len(unregisteredToolCalls))
	}
	// Verify unregistered tool call is present
	var foundUnregistered bool
	for _, call := range unregisteredToolCalls {
		if call.Name == "suggest_something" {
			foundUnregistered = true
		}
	}
	if !foundUnregistered {
		t.Errorf("Expected suggest_something tool call to be present")
	}
}
