package llm

import (
	"testing"
)

func TestSplitParts_WithReasoningContent(t *testing.T) {
	// Test that splitParts correctly extracts reasoning content from parts
	parts := []Part{
		{
			Type:             PartText,
			Text:             "Hello world",
			ReasoningContent: "I need to think about this carefully",
		},
	}

	text, toolCalls, reasoning := splitParts(parts)

	if text != "Hello world" {
		t.Errorf("expected text 'Hello world', got %q", text)
	}
	if len(toolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(toolCalls))
	}
	if reasoning != "I need to think about this carefully" {
		t.Errorf("expected reasoning 'I need to think about this carefully', got %q", reasoning)
	}
}

func TestSplitParts_WithToolCallsAndReasoning(t *testing.T) {
	// Test that splitParts handles both tool calls and reasoning
	parts := []Part{
		{
			Type:             PartText,
			Text:             "Let me help you with that",
			ReasoningContent: "The user wants to list files",
		},
		{
			Type: PartToolCall,
			ToolCall: &ToolCall{
				ID:        "call-123",
				Name:      "list_files",
				Arguments: []byte(`{"path": "."}`),
			},
		},
	}

	text, toolCalls, reasoning := splitParts(parts)

	if text != "Let me help you with that" {
		t.Errorf("expected text 'Let me help you with that', got %q", text)
	}
	if len(toolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].ID != "call-123" {
		t.Errorf("expected tool call ID 'call-123', got %q", toolCalls[0].ID)
	}
	if reasoning != "The user wants to list files" {
		t.Errorf("expected reasoning 'The user wants to list files', got %q", reasoning)
	}
}

func TestBuildCompatMessages_WithReasoningContent(t *testing.T) {
	// Test that buildCompatMessages includes reasoning_content in assistant messages
	messages := []Message{
		{
			Role: RoleUser,
			Parts: []Part{
				{Type: PartText, Text: "What files are here?"},
			},
		},
		{
			Role: RoleAssistant,
			Parts: []Part{
				{
					Type:             PartText,
					Text:             "Let me check",
					ReasoningContent: "User wants to see directory contents",
				},
				{
					Type: PartToolCall,
					ToolCall: &ToolCall{
						ID:        "call-456",
						Name:      "list_files",
						Arguments: []byte(`{"path": "."}`),
					},
				},
			},
		},
		{
			Role: RoleTool,
			Parts: []Part{
				{
					Type: PartToolResult,
					ToolResult: &ToolResult{
						ID:      "call-456",
						Name:    "list_files",
						Content: "file1\nfile2",
					},
				},
			},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(oaiMsgs))
	}

	// Check user message
	if oaiMsgs[0].Role != "user" {
		t.Errorf("expected first message role 'user', got %q", oaiMsgs[0].Role)
	}

	// Check assistant message with reasoning
	assistantMsg := oaiMsgs[1]
	if assistantMsg.Role != "assistant" {
		t.Errorf("expected second message role 'assistant', got %q", assistantMsg.Role)
	}
	if assistantMsg.ReasoningContent != "User wants to see directory contents" {
		t.Errorf("expected reasoning_content 'User wants to see directory contents', got %q", assistantMsg.ReasoningContent)
	}
	if len(assistantMsg.ToolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(assistantMsg.ToolCalls))
	}

	if oaiMsgs[2].Role != "tool" {
		t.Errorf("expected third message role 'tool', got %q", oaiMsgs[2].Role)
	}
}

func TestBuildCompatMessages_NoReasoningContent(t *testing.T) {
	messages := []Message{
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "Hello"}},
		},
		{
			Role:  RoleAssistant,
			Parts: []Part{{Type: PartText, Text: "Hi there!"}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)
	if len(oaiMsgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(oaiMsgs))
	}
	if oaiMsgs[1].ReasoningContent != "" {
		t.Errorf("expected empty reasoning_content, got %q", oaiMsgs[1].ReasoningContent)
	}
}

func TestBuildCompatMessages_ToolResultStructuredImageParts(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{{
				Type: PartToolCall,
				ToolCall: &ToolCall{
					ID:        "call-1",
					Name:      "view_image",
					Arguments: []byte(`{"path":"wow.png"}`),
				},
			}},
		},
		{
			Role: RoleTool,
			Parts: []Part{{
				Type: PartToolResult,
				ToolResult: &ToolResult{
					ID:      "call-1",
					Name:    "view_image",
					Content: "Image loaded",
					ContentParts: []ToolContentPart{
						{Type: ToolContentPartText, Text: "Image loaded"},
						{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
						{Type: ToolContentPartText, Text: "done"},
					},
				},
			}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)
	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages (assistant + tool + user multimodal), got %d", len(oaiMsgs))
	}
	if oaiMsgs[0].Role != "assistant" || len(oaiMsgs[0].ToolCalls) != 1 {
		t.Fatalf("unexpected assistant tool-call message: %#v", oaiMsgs[0])
	}
	if oaiMsgs[1].Role != "tool" || oaiMsgs[1].Content != "Image loadeddone" {
		t.Fatalf("unexpected tool message: %#v", oaiMsgs[1])
	}
	if oaiMsgs[2].Role != "user" {
		t.Fatalf("expected third message role user, got %q", oaiMsgs[2].Role)
	}
	parts, ok := oaiMsgs[2].Content.([]oaiContentPart)
	if !ok {
		t.Fatalf("expected user content []oaiContentPart, got %T", oaiMsgs[2].Content)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 content parts, got %d", len(parts))
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("expected second content part image_url, got %#v", parts[1])
	}
}

func TestNormalizeSchemaForOpenAI_FreeFormMapProperty(t *testing.T) {
	// Regression: env parameter uses additionalProperties: {type: string} to represent
	// a free-form string map. The normalizer must not clobber it with false.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "Shell command to execute",
			},
			"env": map[string]interface{}{
				"type":                 "object",
				"description":          "Environment variables",
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}

	result := normalizeSchemaForOpenAI(schema)

	props, ok := result["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("expected properties map")
	}

	envSchema, ok := props["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to be a map")
	}

	// additionalProperties on env must remain a schema map, not false
	ap := envSchema["additionalProperties"]
	apMap, ok := ap.(map[string]interface{})
	if !ok {
		t.Fatalf("expected env.additionalProperties to remain a schema map, got %T (%v)", ap, ap)
	}
	if apMap["type"] != "string" {
		t.Errorf("expected env.additionalProperties.type = string, got %v", apMap["type"])
	}

	// Outer object must still have additionalProperties: false
	if result["additionalProperties"] != false {
		t.Errorf("expected outer additionalProperties = false, got %v", result["additionalProperties"])
	}

	// All properties must appear in required (OpenAI strict mode)
	required, ok := result["required"].([]string)
	if !ok {
		t.Fatal("expected required to be []string")
	}
	requiredSet := make(map[string]bool)
	for _, k := range required {
		requiredSet[k] = true
	}
	for k := range props {
		if !requiredSet[k] {
			t.Errorf("expected %q in required array, not found", k)
		}
	}
}

func TestNormalizeSchemaForOpenAI_RegularObjectGetsAdditionalPropertiesFalse(t *testing.T) {
	// Regular nested object (no additionalProperties set) should get additionalProperties: false
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"options": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"verbose": map[string]interface{}{"type": "boolean"},
				},
			},
		},
	}

	result := normalizeSchemaForOpenAI(schema)

	props := result["properties"].(map[string]interface{})
	optionsSchema := props["options"].(map[string]interface{})

	if optionsSchema["additionalProperties"] != false {
		t.Errorf("expected nested object to get additionalProperties: false, got %v", optionsSchema["additionalProperties"])
	}
}

func TestBuildCompatMessages_DropsDanglingToolCalls(t *testing.T) {
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

	oaiMsgs := buildCompatMessages(messages)
	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(oaiMsgs))
	}

	assistant := oaiMsgs[1]
	if assistant.Role != "assistant" {
		t.Fatalf("expected assistant role, got %q", assistant.Role)
	}
	if len(assistant.ToolCalls) != 0 {
		t.Fatalf("expected dangling tool calls to be removed, got %d", len(assistant.ToolCalls))
	}
	if assistant.Content != "Working on it" {
		t.Fatalf("expected assistant text to be preserved, got %v", assistant.Content)
	}
}
