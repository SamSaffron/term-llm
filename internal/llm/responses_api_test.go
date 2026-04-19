package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func drainStreamToDone(t *testing.T, stream Stream) {
	t.Helper()
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return
		}
		if recvErr != nil {
			t.Fatalf("stream recv failed: %v", recvErr)
		}
		if event.Type == EventError {
			t.Fatalf("stream returned error event: %v", event.Err)
		}
		if event.Type == EventDone {
			return
		}
	}
}

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

func TestBuildResponsesInput_ToolResultStructuredImageParts(t *testing.T) {
	messages := []Message{
		{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{
			ID:        "call_img",
			Name:      "view_image",
			Arguments: json.RawMessage(`{"file_path":"image.png"}`),
		}}}},
		{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{
			ID:      "call_img",
			Name:    "view_image",
			Content: "Image loaded",
			ContentParts: []ToolContentPart{
				{Type: ToolContentPartText, Text: "Image loaded"},
				{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
				{Type: ToolContentPartText, Text: "done"},
			},
		}}}},
	}

	input := BuildResponsesInput(messages)
	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}
	if input[1].Type != "function_call_output" {
		t.Fatalf("expected second input item function_call_output, got %q", input[1].Type)
	}
	if input[1].Output != "Image loadeddone" {
		t.Fatalf("expected function_call_output text from structured text parts, got %q", input[1].Output)
	}
	if input[2].Type != "message" || input[2].Role != "user" {
		t.Fatalf("expected third input item user message, got %#v", input[2])
	}
	parts, ok := input[2].Content.([]ResponsesContentPart)
	if !ok {
		t.Fatalf("expected message content []ResponsesContentPart, got %T", input[2].Content)
	}
	// Synthetic user message should contain ONLY the image part (text is
	// already in function_call_output and should not be duplicated).
	if len(parts) != 1 {
		t.Fatalf("expected 1 image-only content part, got %d: %+v", len(parts), parts)
	}
	if parts[0].Type != "input_image" {
		t.Fatalf("expected input_image part, got %#v", parts[0])
	}
}

func TestBuildResponsesInput_AssistantReasoningReplay(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{
				{
					Type:                      PartText,
					Text:                      "Final answer",
					ReasoningContent:          "I reviewed the repository first.",
					ReasoningItemID:           "rs_123",
					ReasoningEncryptedContent: "enc_abc",
				},
			},
		},
	}

	input := BuildResponsesInput(messages)
	if len(input) != 2 {
		t.Fatalf("expected 2 input items (reasoning + message), got %d", len(input))
	}

	var reasoningItem *ResponsesInputItem
	var assistantMessage *ResponsesInputItem
	for i := range input {
		switch input[i].Type {
		case "reasoning":
			reasoningItem = &input[i]
		case "message":
			if input[i].Role == "assistant" {
				assistantMessage = &input[i]
			}
		}
	}

	if reasoningItem == nil {
		t.Fatal("expected reasoning input item")
	}
	if reasoningItem.ID != "rs_123" {
		t.Errorf("expected reasoning id rs_123, got %q", reasoningItem.ID)
	}
	if reasoningItem.EncryptedContent != "enc_abc" {
		t.Errorf("expected encrypted_content enc_abc, got %q", reasoningItem.EncryptedContent)
	}
	if reasoningItem.Summary == nil {
		t.Fatal("expected reasoning summary to be present")
	}
	if len(*reasoningItem.Summary) != 1 {
		t.Fatalf("expected one reasoning summary part, got %d", len(*reasoningItem.Summary))
	}
	if (*reasoningItem.Summary)[0].Type != "summary_text" {
		t.Errorf("expected summary type summary_text, got %q", (*reasoningItem.Summary)[0].Type)
	}
	if (*reasoningItem.Summary)[0].Text != "I reviewed the repository first." {
		t.Errorf("unexpected summary text: %q", (*reasoningItem.Summary)[0].Text)
	}

	if assistantMessage == nil {
		t.Fatal("expected assistant message input item")
	}
	if assistantMessage.Content != "Final answer" {
		t.Errorf("expected assistant message content Final answer, got %#v", assistantMessage.Content)
	}
}

func TestBuildResponsesInput_AssistantReasoningReplayEmptySummary(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{
				{
					Type:                      PartText,
					Text:                      "Answer text",
					ReasoningItemID:           "rs_empty",
					ReasoningEncryptedContent: "enc_empty",
				},
			},
		},
	}

	input := BuildResponsesInput(messages)
	if len(input) != 2 {
		t.Fatalf("expected 2 input items (reasoning + message), got %d", len(input))
	}

	var reasoningItem *ResponsesInputItem
	for i := range input {
		if input[i].Type == "reasoning" {
			reasoningItem = &input[i]
			break
		}
	}
	if reasoningItem == nil {
		t.Fatal("expected reasoning item")
	}
	if reasoningItem.Summary == nil {
		t.Fatal("expected summary field to be present even when empty")
	}
	if len(*reasoningItem.Summary) != 0 {
		t.Fatalf("expected empty summary, got %d parts", len(*reasoningItem.Summary))
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

func TestResponsesClientStream_SendsSessionHeaderAndPromptCacheKey(t *testing.T) {
	type capturedRequest struct {
		SessionID      string
		PromptCacheKey string
	}

	captured := make(chan capturedRequest, 1)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var payload struct {
				PromptCacheKey string `json:"prompt_cache_key"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)

			captured <- capturedRequest{
				SessionID:      r.Header.Get("session_id"),
				PromptCacheKey: payload.PromptCacheKey,
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n",
				)),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer test-token" },
		HTTPClient:    httpClient,
	}

	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model: "gpt-5.2",
		Input: []ResponsesInputItem{
			{Type: "message", Role: "user", Content: "hello"},
		},
		Stream:         true,
		SessionID:      "session-123",
		PromptCacheKey: "session-123",
	}, false)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	select {
	case req := <-captured:
		if req.SessionID != "session-123" {
			t.Fatalf("expected session_id header 'session-123', got %q", req.SessionID)
		}
		if req.PromptCacheKey != "session-123" {
			t.Fatalf("expected prompt_cache_key 'session-123', got %q", req.PromptCacheKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request capture")
	}
}

func TestResponsesClientStream_CloseReturnsPromptlyWhenConsumerStopsDraining(t *testing.T) {
	var sse strings.Builder
	for i := 0; i < 32; i++ {
		fmt.Fprintf(&sse, "event: response.output_text.delta\ndata: {\"delta\":\"chunk-%02d\"}\n\n", i)
	}

	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(strings.NewReader(sse.String())),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer test-token" },
		HTTPClient:    httpClient,
	}

	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model: "gpt-5.2",
		Input: []ResponsesInputItem{
			{Type: "message", Role: "user", Content: "hello"},
		},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}

	closed := make(chan error, 1)
	go func() {
		closed <- stream.Close()
	}()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("stream.Close() failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream.Close() timed out after consumer stopped draining")
	}
}

func TestOpenAIProviderStream_UsesSessionIDForResponsesCaching(t *testing.T) {
	type capturedRequest struct {
		SessionID      string
		PromptCacheKey string
	}

	captured := make(chan capturedRequest, 1)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var payload struct {
				PromptCacheKey string `json:"prompt_cache_key"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)

			captured <- capturedRequest{
				SessionID:      r.Header.Get("session_id"),
				PromptCacheKey: payload.PromptCacheKey,
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_openai\"}}\n\n",
				)),
			}, nil
		}),
	}

	provider := &OpenAIProvider{
		apiKey: "test-key",
		model:  "gpt-5.2",
		responsesClient: &ResponsesClient{
			BaseURL:       "https://example.test/v1/responses",
			GetAuthHeader: func() string { return "Bearer test-key" },
			HTTPClient:    httpClient,
		},
	}

	stream, err := provider.Stream(context.Background(), Request{
		Model:     "gpt-5.2",
		SessionID: "openai-session-1",
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
		},
	})
	if err != nil {
		t.Fatalf("openai stream failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	select {
	case req := <-captured:
		if req.SessionID != "openai-session-1" {
			t.Fatalf("expected session_id header 'openai-session-1', got %q", req.SessionID)
		}
		if req.PromptCacheKey != "openai-session-1" {
			t.Fatalf("expected prompt_cache_key 'openai-session-1', got %q", req.PromptCacheKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request capture")
	}
}

func TestCopilotStreamResponses_UsesSessionIDForResponsesCaching(t *testing.T) {
	type capturedRequest struct {
		SessionID      string
		PromptCacheKey string
	}

	captured := make(chan capturedRequest, 1)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var payload struct {
				PromptCacheKey string `json:"prompt_cache_key"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)

			captured <- capturedRequest{
				SessionID:      r.Header.Get("session_id"),
				PromptCacheKey: payload.PromptCacheKey,
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_copilot\"}}\n\n",
				)),
			}, nil
		}),
	}

	provider := &CopilotProvider{
		model:        "gpt-5.2",
		apiBaseURL:   "https://api.githubcopilot.com",
		sessionToken: "copilot-session-token",
		responsesClient: &ResponsesClient{
			BaseURL:       "https://example.test/v1/responses",
			GetAuthHeader: func() string { return "Bearer copilot-session-token" },
			HTTPClient:    httpClient,
		},
	}

	stream, err := provider.streamResponses(context.Background(), Request{
		Model:     "gpt-5.2",
		SessionID: "copilot-session-1",
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
		},
	}, "gpt-5.2")
	if err != nil {
		t.Fatalf("copilot stream failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	select {
	case req := <-captured:
		if req.SessionID != "copilot-session-1" {
			t.Fatalf("expected session_id header 'copilot-session-1', got %q", req.SessionID)
		}
		if req.PromptCacheKey != "copilot-session-1" {
			t.Fatalf("expected prompt_cache_key 'copilot-session-1', got %q", req.PromptCacheKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request capture")
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

func TestBuildResponsesInput_ConvertsDanglingToolCalls(t *testing.T) {
	messages := []Message{
		{
			Role: RoleUser,
			Parts: []Part{
				{Type: PartText, Text: "Run a tool"},
			},
		},
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartText, Text: "Working on it"},
				{
					Type: PartToolCall,
					ToolCall: &ToolCall{
						ID:        "call-1",
						Name:      "shell",
						Arguments: []byte(`{"command":"sleep 10"}`),
					},
				},
			},
		},
		{
			Role: RoleUser,
			Parts: []Part{
				{Type: PartText, Text: "new request"},
			},
		},
	}

	items := BuildResponsesInput(messages)

	// No function_call items should remain
	for _, item := range items {
		if item.Type == "function_call" {
			t.Fatalf("expected no function_call items, found one: %+v", item)
		}
	}

	// Marshal to JSON and check assistant text is preserved with interrupted stub
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("failed to marshal items: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "Working on it") {
		t.Fatalf("expected original assistant text to be preserved, got: %s", s)
	}
	if !strings.Contains(s, "[tool call interrupted") {
		t.Fatalf("expected [tool call interrupted stub, got: %s", s)
	}
}

func TestFilterToNewInput_ToolFollowUpReturnsOnlyNewToolOutputs(t *testing.T) {
	input := []ResponsesInputItem{
		{Type: "message", Role: "developer", Content: "Be concise"},
		{Type: "message", Role: "user", Content: "old question"},
		{Type: "message", Role: "assistant", Content: "I'll check"},
		{Type: "function_call", CallID: "call_1", Name: "shell", Arguments: `{"command":"pwd"}`},
		{Type: "function_call_output", CallID: "call_1", Output: "/root/source/term-llm"},
	}

	got := filterToNewInput(input)

	if len(got) != 1 {
		t.Fatalf("expected only trailing tool output, got %d items: %+v", len(got), got)
	}
	if got[0].Type != "function_call_output" || got[0].CallID != "call_1" {
		t.Fatalf("expected trailing function_call_output for call_1, got %+v", got[0])
	}
}

func TestFilterToNewInput_ToolFollowUpPreservesTrailingOutputsAndUserMessages(t *testing.T) {
	input := []ResponsesInputItem{
		{Type: "message", Role: "developer", Content: "Be concise"},
		{Type: "message", Role: "user", Content: "describe this image"},
		{Type: "function_call", CallID: "call_img", Name: "view_image", Arguments: `{"path":"img.png"}`},
		{Type: "function_call_output", CallID: "call_img", Output: "loaded"},
		{Type: "message", Role: "user", Content: []ResponsesContentPart{{Type: "input_image", ImageURL: "data:image/png;base64,abc"}}},
	}

	got := filterToNewInput(input)

	if len(got) != 2 {
		t.Fatalf("expected trailing tool output and synthetic user message, got %d items: %+v", len(got), got)
	}
	if got[0].Type != "function_call_output" || got[0].CallID != "call_img" {
		t.Fatalf("expected first item function_call_output for call_img, got %+v", got[0])
	}
	if got[1].Type != "message" || got[1].Role != "user" {
		t.Fatalf("expected second item trailing user message, got %+v", got[1])
	}
}

func TestResponsesClientStream_Retries404WithFullHistory(t *testing.T) {
	type capturedRequest struct {
		PreviousResponseID string               `json:"previous_response_id"`
		Input              []ResponsesInputItem `json:"input"`
	}

	callCount := 0
	requests := make([]capturedRequest, 0, 2)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			defer r.Body.Close()

			var payload capturedRequest
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to read request body: %w", err)
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return nil, fmt.Errorf("failed to decode request body: %w", err)
			}
			requests = append(requests, payload)

			if callCount == 1 {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Status:     "404 Not Found",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"previous_response_id not found"}`)),
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:        "https://example.test/v1/responses",
		GetAuthHeader:  func() string { return "Bearer test-token" },
		HTTPClient:     httpClient,
		LastResponseID: "resp_prev",
	}

	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model: "gpt-5.2",
		Input: []ResponsesInputItem{
			{Type: "message", Role: "developer", Content: "Be concise"},
			{Type: "message", Role: "user", Content: "old question"},
			{Type: "message", Role: "assistant", Content: "old answer"},
			{Type: "message", Role: "user", Content: "new question"},
		},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("expected stream to succeed after 404 retry, got error: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if callCount != 2 {
		t.Fatalf("expected 2 HTTP calls (initial + retry), got %d", callCount)
	}
	if len(requests) != 2 {
		t.Fatalf("expected 2 captured requests, got %d", len(requests))
	}

	if requests[0].PreviousResponseID != "resp_prev" {
		t.Fatalf("expected initial previous_response_id resp_prev, got %q", requests[0].PreviousResponseID)
	}
	if len(requests[0].Input) != 1 {
		t.Fatalf("expected initial request to send only new input, got %d items", len(requests[0].Input))
	}
	if requests[0].Input[0].Content != "new question" {
		t.Fatalf("expected initial request to send latest user message, got %#v", requests[0].Input[0].Content)
	}

	if requests[1].PreviousResponseID != "" {
		t.Fatalf("expected retry request to clear previous_response_id, got %q", requests[1].PreviousResponseID)
	}
	if len(requests[1].Input) != 4 {
		t.Fatalf("expected retry request to restore full history, got %d items", len(requests[1].Input))
	}
	if requests[1].Input[0].Role != "developer" || requests[1].Input[1].Content != "old question" || requests[1].Input[2].Content != "old answer" || requests[1].Input[3].Content != "new question" {
		t.Fatalf("expected retry request to preserve full history, got %+v", requests[1].Input)
	}
	if client.LastResponseID != "" {
		t.Fatalf("expected client LastResponseID to be cleared after 404 retry, got %q", client.LastResponseID)
	}
}

func TestResponsesClient_OnAuthRetry_RefreshesAndRetries(t *testing.T) {
	callCount := 0
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			if callCount == 1 {
				// First call returns 401
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Status:     "401 Unauthorized",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid token"}`)),
				}, nil
			}
			// Second call (after retry) succeeds
			if auth := r.Header.Get("Authorization"); auth != "Bearer refreshed-token" {
				t.Errorf("expected refreshed token on retry, got %q", auth)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_retry\"}}\n\n",
				)),
			}, nil
		}),
	}

	token := "expired-token"
	retryCalled := false
	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer " + token },
		HTTPClient:    httpClient,
		OnAuthRetry: func(_ context.Context) error {
			retryCalled = true
			token = "refreshed-token"
			return nil
		},
	}

	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "test-model",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hello"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("expected stream to succeed after auth retry, got error: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if !retryCalled {
		t.Fatal("expected OnAuthRetry to be called")
	}
	if callCount != 2 {
		t.Fatalf("expected 2 HTTP calls (initial + retry), got %d", callCount)
	}
}

func TestResponsesClient_OnAuthRetry_FailureReturnsError(t *testing.T) {
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid token"}`)),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer bad-token" },
		HTTPClient:    httpClient,
		OnAuthRetry: func(_ context.Context) error {
			return fmt.Errorf("re-authentication failed: user cancelled")
		},
	}

	_, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "test-model",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hello"}},
		Stream: true,
	}, false)
	if err == nil {
		t.Fatal("expected error when OnAuthRetry fails")
	}
	if !strings.Contains(err.Error(), "re-authentication failed") {
		t.Fatalf("expected re-authentication error, got: %v", err)
	}
}

func TestResponsesClient_NoOnAuthRetry_Returns401Error(t *testing.T) {
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid token"}`)),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer bad-token" },
		HTTPClient:    httpClient,
		// No OnAuthRetry set
	}

	_, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "test-model",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hello"}},
		Stream: true,
	}, false)
	if err == nil {
		t.Fatal("expected error on 401 without OnAuthRetry")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("expected authentication failed error, got: %v", err)
	}
}

func TestResponsesClientStream_AllowsLargeSSEDataLines(t *testing.T) {
	largeDelta := strings.Repeat("x", 1024*1024+32)
	deltaJSON, err := json.Marshal(struct {
		Delta string `json:"delta"`
	}{Delta: largeDelta})
	if err != nil {
		t.Fatalf("marshal delta event: %v", err)
	}

	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"event: response.output_text.delta\n" +
						"data: " + string(deltaJSON) + "\n\n" +
						"event: response.completed\n" +
						"data: {\"response\":{\"id\":\"resp_large\"}}\n\n" +
						"data: [DONE]\n\n",
				)),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer test-token" },
		HTTPClient:    httpClient,
	}

	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "gpt-5.2",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hello"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()

	var got strings.Builder
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("stream recv failed: %v", recvErr)
		}
		switch event.Type {
		case EventTextDelta:
			got.WriteString(event.Text)
		case EventError:
			t.Fatalf("unexpected error event: %v", event.Err)
		case EventDone:
			if got.String() != largeDelta {
				t.Fatalf("expected %d bytes of text delta, got %d", len(largeDelta), got.Len())
			}
			if client.LastResponseID != "resp_large" {
				t.Fatalf("expected LastResponseID resp_large, got %q", client.LastResponseID)
			}
			return
		}
	}

	t.Fatal("expected EventDone")
}

func TestResponsesClientResetConversationIgnoresLateStreamCompletion(t *testing.T) {
	type capturedRequest struct {
		PreviousResponseID string `json:"previous_response_id"`
	}

	allowCompletion := make(chan struct{})
	callCount := 0
	requests := make([]capturedRequest, 0, 2)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			defer r.Body.Close()

			var payload capturedRequest
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to read request body: %w", err)
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return nil, fmt.Errorf("failed to decode request body: %w", err)
			}
			requests = append(requests, payload)

			if callCount == 1 {
				pr, pw := io.Pipe()
				go func() {
					defer pw.Close()
					_, _ = io.WriteString(pw,
						"event: response.output_text.delta\n"+
							"data: {\"delta\":\"hello\"}\n\n",
					)
					<-allowCompletion
					_, _ = io.WriteString(pw,
						"event: response.completed\n"+
							"data: {\"response\":{\"id\":\"resp_old\"}}\n\n"+
							"data: [DONE]\n\n",
					)
				}()
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       pr,
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer test-token" },
		HTTPClient:    httpClient,
	}

	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "gpt-5.2",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hello"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream recv failed: %v", err)
	}
	if event.Type != EventTextDelta || event.Text != "hello" {
		t.Fatalf("expected initial text delta, got %+v", event)
	}

	client.ResetConversation()
	close(allowCompletion)
	drainStreamToDone(t, stream)

	if client.LastResponseID != "" {
		t.Fatalf("expected ResetConversation to keep LastResponseID cleared, got %q", client.LastResponseID)
	}

	nextStream, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "gpt-5.2",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "new conversation"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("second stream creation failed: %v", err)
	}
	defer nextStream.Close()
	drainStreamToDone(t, nextStream)

	if callCount != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", callCount)
	}
	if len(requests) != 2 {
		t.Fatalf("expected 2 captured requests, got %d", len(requests))
	}
	if requests[1].PreviousResponseID != "" {
		t.Fatalf("expected new conversation request to omit previous_response_id, got %q", requests[1].PreviousResponseID)
	}
}

func assertResponsesAPIErrorBodyTruncated(t *testing.T, err error, prefix string) {
	t.Helper()

	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), prefix) {
		t.Fatalf("expected error %q to contain %q", err.Error(), prefix)
	}
	if !strings.Contains(err.Error(), string(truncatedResponsesAPIErrorBodySuffix)) {
		t.Fatalf("expected error %q to contain truncation suffix", err.Error())
	}
	if strings.Contains(err.Error(), "TAIL_MARKER") {
		t.Fatalf("expected error %q not to include truncated tail marker", err.Error())
	}
}

func TestResponsesClient_Stream_LimitsErrorBody(t *testing.T) {
	body := strings.Repeat("x", maxResponsesAPIErrorBodyBytes+1024) + "TAIL_MARKER"
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Status:     "500 Internal Server Error",
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer test-token" },
		HTTPClient:    httpClient,
	}

	_, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "test-model",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hello"}},
		Stream: true,
	}, false)

	assertResponsesAPIErrorBodyTruncated(t, err, "Responses API error (status 500):")
}

func TestResponsesClientStream_Retry404FailureLimitsErrorBody(t *testing.T) {
	body := strings.Repeat("x", maxResponsesAPIErrorBodyBytes+1024) + "TAIL_MARKER"
	callCount := 0
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			if callCount == 1 {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Status:     "404 Not Found",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"previous_response_id not found"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Status:     "500 Internal Server Error",
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:        "https://example.test/v1/responses",
		GetAuthHeader:  func() string { return "Bearer test-token" },
		HTTPClient:     httpClient,
		LastResponseID: "resp_prev",
	}

	_, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "test-model",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hello"}},
		Stream: true,
	}, false)

	if callCount != 2 {
		t.Fatalf("expected 2 HTTP calls (initial + retry), got %d", callCount)
	}
	assertResponsesAPIErrorBodyTruncated(t, err, "Responses API error (status 500):")
}

func TestResponsesClient_OnAuthRetry_LimitsErrorBodyAfterReauth(t *testing.T) {
	body := strings.Repeat("x", maxResponsesAPIErrorBodyBytes+1024) + "TAIL_MARKER"
	callCount := 0
	token := "expired-token"
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			if callCount == 1 {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Status:     "401 Unauthorized",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid token"}`)),
				}, nil
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer refreshed-token" {
				t.Fatalf("expected refreshed token on retry, got %q", auth)
			}
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	client := &ResponsesClient{
		BaseURL:       "https://example.test/v1/responses",
		GetAuthHeader: func() string { return "Bearer " + token },
		HTTPClient:    httpClient,
		OnAuthRetry: func(_ context.Context) error {
			token = "refreshed-token"
			return nil
		},
	}

	_, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "test-model",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hello"}},
		Stream: true,
	}, false)

	if callCount != 2 {
		t.Fatalf("expected 2 HTTP calls (initial + retry), got %d", callCount)
	}
	assertResponsesAPIErrorBodyTruncated(t, err, "Responses API error after re-auth (status 403):")
}

func TestBuildResponsesInput_DeveloperRole(t *testing.T) {
	messages := []Message{
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "Be concise"}}},
		UserText("Hello"),
		AssistantText("Hi"),
	}

	input := BuildResponsesInput(messages)

	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}
	if input[0].Role != "developer" {
		t.Errorf("expected developer role, got %q", input[0].Role)
	}
	if input[0].Content != "Be concise" {
		t.Errorf("expected developer content 'Be concise', got %v", input[0].Content)
	}
	if input[1].Role != "user" {
		t.Errorf("expected user role, got %q", input[1].Role)
	}
	if input[2].Role != "assistant" {
		t.Errorf("expected assistant role, got %q", input[2].Role)
	}
}

func TestBuildResponsesInputWithInstructions_DeveloperStaysInline(t *testing.T) {
	messages := []Message{
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "You are helpful."}}},
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "Be concise"}}},
		UserText("Hello"),
	}

	instructions, input := BuildResponsesInputWithInstructions(messages)

	// System messages should be extracted to instructions
	if instructions != "You are helpful." {
		t.Fatalf("expected system instructions, got %q", instructions)
	}

	// Developer message should stay inline, not be extracted
	if len(input) != 2 {
		t.Fatalf("expected 2 input items (developer + user), got %d", len(input))
	}
	if input[0].Role != "developer" {
		t.Errorf("expected developer role inline, got %q", input[0].Role)
	}
	if input[0].Content != "Be concise" {
		t.Errorf("expected developer content 'Be concise', got %v", input[0].Content)
	}
	if input[1].Role != "user" {
		t.Errorf("expected user role, got %q", input[1].Role)
	}
}
