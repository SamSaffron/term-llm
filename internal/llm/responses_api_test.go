package llm

import (
	"encoding/json"
	"testing"
)

func TestUseResponsesAPI(t *testing.T) {
	tests := []struct {
		model    string
		expected bool
	}{
		// GPT-5 models should use Responses API
		{"gpt-5", true},
		{"gpt-5.1", true},
		{"gpt-5.2", true},
		{"gpt-5.2-high", true},
		{"GPT-5.2", true}, // Case insensitive

		// Codex models should use Responses API
		{"gpt-5.2-codex", true},
		{"gpt-5.1-codex-max", true},
		{"codex-5.2", true},

		// Reasoning models should use Responses API
		{"o1", true},
		{"o1-mini", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4", true},

		// Older models should use Chat Completions
		{"gpt-4.1", false},
		{"gpt-4o", false},
		{"claude-sonnet-4", false},
		{"claude-opus-4.5", false},
		{"gemini-3-pro", false},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			result := useResponsesAPI(tc.model)
			if result != tc.expected {
				t.Errorf("useResponsesAPI(%q) = %v, want %v", tc.model, result, tc.expected)
			}
		})
	}
}

func TestBuildResponsesInput(t *testing.T) {
	messages := []Message{
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "You are a helpful assistant."}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "Hello"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "Hi there!"}}},
	}

	input := BuildResponsesInput(messages)

	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}

	// System message should be converted to developer role
	if input[0].Role != "developer" {
		t.Errorf("expected system message to have role 'developer', got %q", input[0].Role)
	}
	if input[0].Content != "You are a helpful assistant." {
		t.Errorf("expected system message content 'You are a helpful assistant.', got %v", input[0].Content)
	}

	// User message
	if input[1].Role != "user" {
		t.Errorf("expected user message role 'user', got %q", input[1].Role)
	}

	// Assistant message
	if input[2].Role != "assistant" {
		t.Errorf("expected assistant message role 'assistant', got %q", input[2].Role)
	}
}

func TestBuildResponsesInput_ToolCalls(t *testing.T) {
	messages := []Message{
		{Role: RoleAssistant, Parts: []Part{
			{Type: PartToolCall, ToolCall: &ToolCall{
				ID:        "call_123",
				Name:      "get_weather",
				Arguments: json.RawMessage(`{"location": "NYC"}`),
			}},
		}},
		{Role: RoleTool, Parts: []Part{
			{Type: PartToolResult, ToolResult: &ToolResult{
				ID:      "call_123",
				Name:    "get_weather",
				Content: "Sunny, 72F",
			}},
		}},
	}

	input := BuildResponsesInput(messages)

	if len(input) != 2 {
		t.Fatalf("expected 2 input items, got %d", len(input))
	}

	// Function call
	if input[0].Type != "function_call" {
		t.Errorf("expected function_call type, got %q", input[0].Type)
	}
	if input[0].CallID != "call_123" {
		t.Errorf("expected call_id 'call_123', got %q", input[0].CallID)
	}
	if input[0].Name != "get_weather" {
		t.Errorf("expected name 'get_weather', got %q", input[0].Name)
	}

	// Function call output
	if input[1].Type != "function_call_output" {
		t.Errorf("expected function_call_output type, got %q", input[1].Type)
	}
	if input[1].Output != "Sunny, 72F" {
		t.Errorf("expected output 'Sunny, 72F', got %q", input[1].Output)
	}
}

func TestBuildResponsesTools(t *testing.T) {
	specs := []ToolSpec{
		{
			Name:        "get_weather",
			Description: "Get the current weather",
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"location": map[string]interface{}{
						"type":        "string",
						"description": "City name",
					},
				},
			},
		},
	}

	tools := BuildResponsesTools(specs)

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool, ok := tools[0].(ResponsesTool)
	if !ok {
		t.Fatalf("expected ResponsesTool type")
	}

	if tool.Type != "function" {
		t.Errorf("expected type 'function', got %q", tool.Type)
	}
	if tool.Name != "get_weather" {
		t.Errorf("expected name 'get_weather', got %q", tool.Name)
	}
	if tool.Description != "Get the current weather" {
		t.Errorf("expected description 'Get the current weather', got %q", tool.Description)
	}
	if !tool.Strict {
		t.Error("expected Strict to be true")
	}

	// Check that schema normalization added required and additionalProperties
	if _, ok := tool.Parameters["required"]; !ok {
		t.Error("expected 'required' field to be added by normalization")
	}
	if tool.Parameters["additionalProperties"] != false {
		t.Error("expected 'additionalProperties' to be false")
	}
}

func TestBuildResponsesToolChoice(t *testing.T) {
	tests := []struct {
		choice   ToolChoice
		expected interface{}
	}{
		{ToolChoice{Mode: ToolChoiceAuto}, "auto"},
		{ToolChoice{Mode: ToolChoiceNone}, "none"},
		{ToolChoice{Mode: ToolChoiceRequired}, "required"},
	}

	for _, tc := range tests {
		t.Run(string(tc.choice.Mode), func(t *testing.T) {
			result := BuildResponsesToolChoice(tc.choice)
			if result != tc.expected {
				t.Errorf("BuildResponsesToolChoice(%v) = %v, want %v", tc.choice, result, tc.expected)
			}
		})
	}
}

func TestBuildResponsesToolChoice_SpecificFunction(t *testing.T) {
	choice := ToolChoice{Mode: ToolChoiceName, Name: "get_weather"}
	result := BuildResponsesToolChoice(choice)

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if m["type"] != "function" {
		t.Errorf("expected type 'function', got %v", m["type"])
	}
	if m["name"] != "get_weather" {
		t.Errorf("expected name 'get_weather', got %v", m["name"])
	}
}

func TestResponsesToolState_TrackByOutputIndex(t *testing.T) {
	// This test verifies that tool state tracking works when using output_index
	// (which is stable across events) rather than item_id (which can differ).
	// This is the fix for Copilot where item IDs differ between added/delta/done events.
	state := newResponsesToolState()

	// Simulate events with output_index=1
	// In real Copilot usage, the item_id differs between events, but output_index is stable
	state.StartCall(1, "call_abc123", "web_search")

	// Append arguments using output_index (not item_id which would differ)
	state.AppendArguments(1, `{"query":`)
	state.AppendArguments(1, `"hello"}`)

	// Finish the call
	state.FinishCall(1, "call_abc123", "web_search", "")

	calls := state.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	call := calls[0]
	if call.ID != "call_abc123" {
		t.Errorf("expected call ID 'call_abc123', got %q", call.ID)
	}
	if call.Name != "web_search" {
		t.Errorf("expected name 'web_search', got %q", call.Name)
	}
	if string(call.Arguments) != `{"query":"hello"}` {
		t.Errorf("expected arguments '{\"query\":\"hello\"}', got %q", string(call.Arguments))
	}
}

func TestResponsesToolState_FinishCallWithFinalArgs(t *testing.T) {
	// Test that FinishCall can override streamed args with final args from done event
	state := newResponsesToolState()

	state.StartCall(1, "call_abc", "test_func")
	state.AppendArguments(1, `{"partial`)

	// Done event provides complete final arguments
	state.FinishCall(1, "call_abc", "test_func", `{"complete":"args"}`)

	calls := state.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	// Final args should override the partial streamed args
	if string(calls[0].Arguments) != `{"complete":"args"}` {
		t.Errorf("expected final args to override, got %q", string(calls[0].Arguments))
	}
}

func TestResponsesToolState_FinishCallCreatesNewEntry(t *testing.T) {
	// Test that FinishCall can create a new entry if StartCall was never received
	// This handles edge cases where only the done event is received
	state := newResponsesToolState()

	// Only call FinishCall without prior StartCall (simulates missing added event)
	state.FinishCall(1, "call_xyz", "search", `{"query":"test"}`)

	calls := state.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	call := calls[0]
	if call.ID != "call_xyz" {
		t.Errorf("expected call ID 'call_xyz', got %q", call.ID)
	}
	if call.Name != "search" {
		t.Errorf("expected name 'search', got %q", call.Name)
	}
	if string(call.Arguments) != `{"query":"test"}` {
		t.Errorf("expected arguments, got %q", string(call.Arguments))
	}
}

func TestResponsesToolState_MultipleToolCalls(t *testing.T) {
	// Test tracking multiple concurrent tool calls with different output_index values
	state := newResponsesToolState()

	// Start two tool calls
	state.StartCall(1, "call_1", "search")
	state.StartCall(2, "call_2", "read")

	// Arguments come interleaved (as they might in parallel tool calls)
	state.AppendArguments(1, `{"q":"a"}`)
	state.AppendArguments(2, `{"url":"b"}`)

	// Finish both
	state.FinishCall(1, "call_1", "search", "")
	state.FinishCall(2, "call_2", "read", "")

	calls := state.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}

	// Verify each call has correct data
	if calls[0].ID != "call_1" || string(calls[0].Arguments) != `{"q":"a"}` {
		t.Errorf("call 0 mismatch: %+v", calls[0])
	}
	if calls[1].ID != "call_2" || string(calls[1].Arguments) != `{"url":"b"}` {
		t.Errorf("call 1 mismatch: %+v", calls[1])
	}
}
