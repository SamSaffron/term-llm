package llm

import (
	"strings"
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

func TestBuildResponsesTools_NormalizesFreeFormMapProperty(t *testing.T) {
	specs := []ToolSpec{{
		Name: "shell",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string"},
				"env": map[string]interface{}{
					"type":                 "object",
					"description":          "Environment variables",
					"additionalProperties": map[string]interface{}{"type": "string"},
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}}
	tools := BuildResponsesTools(specs)
	tool := tools[0].(ResponsesTool)
	props := tool.Parameters["properties"].(map[string]interface{})

	// command must remain unchanged
	if _, ok := props["command"]; !ok {
		t.Error("expected command to remain in strict schema")
	}

	// env must be present but transformed to an array of key/value objects
	envSchema, ok := props["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to be present and a map")
	}
	if envSchema["type"] != "array" {
		t.Errorf("expected env to be transformed to array type, got %v", envSchema["type"])
	}
	if envSchema["description"] != "Environment variables" {
		t.Errorf("expected description to be preserved, got %v", envSchema["description"])
	}
	items, ok := envSchema["items"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items to be a map")
	}
	if items["type"] != "object" {
		t.Errorf("expected env.items.type to be object, got %v", items["type"])
	}
	itemProps, ok := items["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items.properties to be a map")
	}
	if _, ok := itemProps["key"]; !ok {
		t.Error("expected env.items.properties.key to exist")
	}
	valueSchema, ok := itemProps["value"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items.properties.value to be a map")
	}
	if valueSchema["type"] != "string" {
		t.Errorf("expected env.items.properties.value.type to be string (original additionalProperties schema), got %v", valueSchema["type"])
	}
	if items["additionalProperties"] != false {
		t.Errorf("expected env.items.additionalProperties to be false, got %v", items["additionalProperties"])
	}
}

func TestNormalizeFreeFormMapProperties_PreservesNonStringValueType(t *testing.T) {
	// A free-form map whose values are integers, not strings.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"counts": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": map[string]interface{}{"type": "integer"},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	countsSchema := props["counts"].(map[string]interface{})
	if countsSchema["type"] != "array" {
		t.Fatalf("expected counts to be transformed to array, got %v", countsSchema["type"])
	}
	items := countsSchema["items"].(map[string]interface{})
	itemProps := items["properties"].(map[string]interface{})
	valueSchema := itemProps["value"].(map[string]interface{})
	if valueSchema["type"] != "integer" {
		t.Errorf("expected value type to preserve original additionalProperties type 'integer', got %v", valueSchema["type"])
	}
}

func TestNormalizeFreeFormMapProperties_TraversesItems(t *testing.T) {
	// A free-form map nested inside an array's items schema.
	schema := map[string]interface{}{
		"type": "array",
		"items": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"env": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": map[string]interface{}{"type": "string"},
				},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	items := result["items"].(map[string]interface{})
	itemProps := items["properties"].(map[string]interface{})
	envSchema, ok := itemProps["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to exist inside items.properties")
	}
	if envSchema["type"] != "array" {
		t.Errorf("expected env inside items to be transformed to array, got %v", envSchema["type"])
	}
}

func TestNormalizeFreeFormMapProperties_AnyOfFreeFormMap(t *testing.T) {
	// A property whose schema is an anyOf where one branch is a free-form map.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"meta": map[string]interface{}{
				"anyOf": []interface{}{
					map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					map[string]interface{}{"type": "null"},
				},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	metaSchema := props["meta"].(map[string]interface{})
	anyOf := metaSchema["anyOf"].([]interface{})

	// First anyOf branch (was a free-form map) must be converted to array.
	firstBranch, ok := anyOf[0].(map[string]interface{})
	if !ok {
		t.Fatal("expected first anyOf branch to be a map")
	}
	if firstBranch["type"] != "array" {
		t.Errorf("expected free-form map in anyOf to be converted to array, got %v", firstBranch["type"])
	}

	// Second anyOf branch (null) must be unchanged.
	secondBranch, ok := anyOf[1].(map[string]interface{})
	if !ok {
		t.Fatal("expected second anyOf branch to be a map")
	}
	if secondBranch["type"] != "null" {
		t.Errorf("expected null branch to be unchanged, got %v", secondBranch["type"])
	}
}

func TestNormalizeFreeFormMapProperties_PreservesMetadata(t *testing.T) {
	// Metadata fields beyond description (title, default) must survive conversion.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"tags": map[string]interface{}{
				"type":                 "object",
				"description":          "Tag values",
				"title":                "Tags",
				"default":              map[string]interface{}{},
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	tagsSchema := props["tags"].(map[string]interface{})
	if tagsSchema["type"] != "array" {
		t.Fatalf("expected tags to be converted to array, got %v", tagsSchema["type"])
	}
	if tagsSchema["description"] != "Tag values" {
		t.Errorf("expected description to be preserved, got %v", tagsSchema["description"])
	}
	if tagsSchema["title"] != "Tags" {
		t.Errorf("expected title to be preserved, got %v", tagsSchema["title"])
	}
	if tagsSchema["default"] == nil {
		t.Error("expected default to be preserved")
	}
}

func TestBuildCompatMessages_ConvertsDanglingToolCalls(t *testing.T) {
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
	// Orphaned tool_use is converted to a text stub so the model knows it was interrupted.
	if !strings.Contains(assistant.Content.(string), "Working on it") {
		t.Fatalf("expected original text to be preserved, got %v", assistant.Content)
	}
	if !strings.Contains(assistant.Content.(string), "[tool call interrupted") {
		t.Fatalf("expected interrupted stub in text, got %v", assistant.Content)
	}
}
