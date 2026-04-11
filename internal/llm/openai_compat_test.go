package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDefaultHTTPClient_HasNoOverallTimeout(t *testing.T) {
	if defaultHTTPClient.Timeout != 0 {
		t.Fatalf("expected shared HTTP client timeout to be disabled for streaming requests, got %v", defaultHTTPClient.Timeout)
	}
}

func TestOpenAICompatStream_AllowsLargeSSEDataLines(t *testing.T) {
	largeText := strings.Repeat("a", 1024*1024+1024)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk, err := json.Marshal(oaiChatResponse{
			Choices: []oaiChoice{{
				Delta: &oaiMessage{Content: largeText},
			}},
		})
		if err != nil {
			t.Fatalf("marshal chunk: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte("data: ")); err != nil {
			t.Fatalf("write prefix: %v", err)
		}
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
		if _, err := w.Write([]byte("\n\ndata: [DONE]\n\n")); err != nil {
			t.Fatalf("write done: %v", err)
		}
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var got strings.Builder
	var sawDone bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			got.WriteString(event.Text)
		case EventDone:
			sawDone = true
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if got.String() != largeText {
		t.Fatalf("expected %d bytes of streamed text, got %d", len(largeText), got.Len())
	}
	if !sawDone {
		t.Fatal("expected EventDone")
	}
}

func TestOpenAICompatStream_CloseDoesNotHangWhenConsumerStopsReceiving(t *testing.T) {
	chunk, err := json.Marshal(oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{Content: "x"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}

	wroteChunks := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("expected response writer to implement http.Flusher")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 64; i++ {
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
		close(wroteChunks)
		<-r.Context().Done()
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	select {
	case <-wroteChunks:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE chunks to be written")
	}

	time.Sleep(100 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- stream.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("stream.Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream.Close() blocked while stream goroutine was trying to emit events")
	}
}

func TestOpenAICompatStream_KeepsToolCallsSeparateWhenIndexesAreOmitted(t *testing.T) {
	firstChunk := oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{ToolCalls: []oaiToolCall{{}, {}}},
		}},
	}
	firstChunk.Choices[0].Delta.ToolCalls[0].ID = "call-1"
	firstChunk.Choices[0].Delta.ToolCalls[0].Function.Name = "search"
	firstChunk.Choices[0].Delta.ToolCalls[0].Function.Arguments = `{"query":"wea`
	firstChunk.Choices[0].Delta.ToolCalls[1].ID = "call-2"
	firstChunk.Choices[0].Delta.ToolCalls[1].Function.Name = "fetch"
	firstChunk.Choices[0].Delta.ToolCalls[1].Function.Arguments = `{"url":"https://exa`

	secondChunk := oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{ToolCalls: []oaiToolCall{{}, {}}},
		}},
	}
	secondChunk.Choices[0].Delta.ToolCalls[0].Function.Arguments = `ther"}`
	secondChunk.Choices[0].Delta.ToolCalls[1].Function.Arguments = `mple.com"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []oaiChatResponse{firstChunk, secondChunk} {
			data, err := json.Marshal(chunk)
			if err != nil {
				t.Fatalf("marshal chunk: %v", err)
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				t.Fatalf("write prefix: %v", err)
			}
			if _, err := w.Write(data); err != nil {
				t.Fatalf("write chunk: %v", err)
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				t.Fatalf("write separator: %v", err)
			}
		}
		if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
			t.Fatalf("write done: %v", err)
		}
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var toolCalls []ToolCall
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventToolCall:
			if event.Tool == nil {
				t.Fatal("expected tool call event to include tool")
			}
			toolCalls = append(toolCalls, *event.Tool)
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(toolCalls), toolCalls)
	}
	if toolCalls[0].ID != "call-1" || toolCalls[0].Name != "search" || string(toolCalls[0].Arguments) != `{"query":"weather"}` {
		t.Fatalf("unexpected first tool call: %#v", toolCalls[0])
	}
	if toolCalls[1].ID != "call-2" || toolCalls[1].Name != "fetch" || string(toolCalls[1].Arguments) != `{"url":"https://example.com"}` {
		t.Fatalf("unexpected second tool call: %#v", toolCalls[1])
	}
}

func TestCompatToolState_CallsStaySortedWhenIndexesArePresent(t *testing.T) {
	s := newCompatToolState()

	first := oaiToolCall{Index: intPtr(1), ID: "call-2"}
	first.Function.Name = "second"
	first.Function.Arguments = `{"value":2}`
	s.Add([]oaiToolCall{first})

	second := oaiToolCall{Index: intPtr(0), ID: "call-1"}
	second.Function.Name = "first"
	second.Function.Arguments = `{"value":1}`
	s.Add([]oaiToolCall{second})

	calls := s.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].ID != "call-1" || calls[1].ID != "call-2" {
		t.Fatalf("expected calls to be ordered by explicit indexes, got %#v", calls)
	}
}

func TestCompatToolState_ReusesPositionWhenIndexAppearsAfterOmission(t *testing.T) {
	s := newCompatToolState()

	first := oaiToolCall{ID: "call-1"}
	first.Function.Name = "search"
	first.Function.Arguments = `{"query":"wea`
	s.Add([]oaiToolCall{first})

	second := oaiToolCall{Index: intPtr(0)}
	second.Function.Arguments = `ther"}`
	s.Add([]oaiToolCall{second})

	calls := s.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %#v", len(calls), calls)
	}
	if calls[0].ID != "call-1" || calls[0].Name != "search" || string(calls[0].Arguments) != `{"query":"weather"}` {
		t.Fatalf("unexpected merged call: %#v", calls[0])
	}
}

func intPtr(v int) *int {
	return &v
}

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
	// Synthetic user message should contain ONLY the image part (text is
	// already in the tool result message and should not be duplicated).
	if len(parts) != 1 {
		t.Fatalf("expected 1 image-only content part, got %d: %+v", len(parts), parts)
	}
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Fatalf("expected image_url part, got %#v", parts[0])
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

func TestBuildCompatMessages_DeveloperRolePrependedToUser(t *testing.T) {
	messages := []Message{
		{
			Role:  RoleSystem,
			Parts: []Part{{Type: PartText, Text: "You are helpful."}},
		},
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "Always respond in JSON."}},
		},
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "What is 2+2?"}},
		},
		{
			Role:  RoleAssistant,
			Parts: []Part{{Type: PartText, Text: `{"answer": 4}`}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages (system, user, assistant), got %d", len(oaiMsgs))
	}
	if oaiMsgs[0].Role != "system" {
		t.Errorf("expected first message role 'system', got %q", oaiMsgs[0].Role)
	}
	userContent, ok := oaiMsgs[1].Content.(string)
	if !ok {
		t.Fatalf("expected user content to be string, got %T", oaiMsgs[1].Content)
	}
	if !strings.Contains(userContent, "<developer>") {
		t.Errorf("expected <developer> tag in user message, got %q", userContent)
	}
	if !strings.Contains(userContent, "Always respond in JSON.") {
		t.Errorf("expected developer text in user message, got %q", userContent)
	}
	if !strings.Contains(userContent, "What is 2+2?") {
		t.Errorf("expected original user text preserved, got %q", userContent)
	}
}

func TestBuildCompatMessages_TrailingDeveloperMessage(t *testing.T) {
	messages := []Message{
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "Hello"}},
		},
		{
			Role:  RoleAssistant,
			Parts: []Part{{Type: PartText, Text: "Hi!"}},
		},
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "Be concise from now on."}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, synthetic user), got %d", len(oaiMsgs))
	}
	trailing := oaiMsgs[2]
	if trailing.Role != "user" {
		t.Errorf("expected trailing message role 'user', got %q", trailing.Role)
	}
	content, ok := trailing.Content.(string)
	if !ok {
		t.Fatalf("expected trailing content to be string, got %T", trailing.Content)
	}
	if !strings.Contains(content, "<developer>") || !strings.Contains(content, "Be concise from now on.") {
		t.Errorf("expected developer text in trailing user message, got %q", content)
	}
}

func TestBuildCompatMessages_DeveloperMessageNotDropped(t *testing.T) {
	// Regression: before the fix, RoleDeveloper messages were silently dropped.
	messages := []Message{
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "You must use formal English."}},
		},
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hey whats up"}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 1 {
		t.Fatalf("expected 1 message (merged dev+user), got %d", len(oaiMsgs))
	}
	content, ok := oaiMsgs[0].Content.(string)
	if !ok {
		t.Fatalf("expected content to be string, got %T", oaiMsgs[0].Content)
	}
	if !strings.Contains(content, "You must use formal English.") {
		t.Errorf("developer text was dropped — not found in %q", content)
	}
	if !strings.Contains(content, "hey whats up") {
		t.Errorf("user text was dropped — not found in %q", content)
	}
}

func TestBuildCompatMessages_ConsecutiveDeveloperMessagesPreserved(t *testing.T) {
	messages := []Message{
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "Platform instruction."}},
		},
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "Runtime instruction."}},
		},
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "Hello"}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 1 {
		t.Fatalf("expected 1 message (merged dev+user), got %d", len(oaiMsgs))
	}
	content, ok := oaiMsgs[0].Content.(string)
	if !ok {
		t.Fatalf("expected content to be string, got %T", oaiMsgs[0].Content)
	}
	if !strings.Contains(content, "Platform instruction.") {
		t.Errorf("first developer text was dropped — not found in %q", content)
	}
	if !strings.Contains(content, "Runtime instruction.") {
		t.Errorf("second developer text was dropped — not found in %q", content)
	}
	if !strings.Contains(content, "Hello") {
		t.Errorf("user text was dropped — not found in %q", content)
	}
}

func TestOpenAICompatProviderStreamSendsExplicitParallelToolCallsFalse(t *testing.T) {
	var got struct {
		ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
		Tools             []json.RawMessage `json:"tools,omitempty"`
		Stream            bool              `json:"stream"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := NewOpenAICompatProvider(ts.URL, "test-key", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
		Tools: []ToolSpec{{
			Name:        "echo",
			Description: "Echo input",
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"text": map[string]interface{}{"type": "string"},
				},
			},
		}},
		ParallelToolCalls: false,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if ev.Type == EventDone {
			break
		}
	}

	if got.ParallelToolCalls == nil {
		t.Fatal("expected parallel_tool_calls to be sent explicitly")
	}
	if *got.ParallelToolCalls {
		t.Fatalf("expected parallel_tool_calls=false, got %v", *got.ParallelToolCalls)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(got.Tools))
	}
	if !got.Stream {
		t.Fatal("expected stream=true")
	}
}

func TestOpenAICompatProviderStreamSendsExplicitZeroTemperatureAndTopP(t *testing.T) {
	var got struct {
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
		Stream      bool     `json:"stream"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := NewOpenAICompatProvider(ts.URL, "test-key", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages:       []Message{UserText("hello")},
		Temperature:    0,
		TemperatureSet: true,
		TopP:           0,
		TopPSet:        true,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if ev.Type == EventDone {
			break
		}
	}

	if got.Temperature == nil {
		t.Fatal("expected temperature=0 to be sent explicitly")
	}
	if *got.Temperature != 0 {
		t.Fatalf("expected temperature=0, got %v", *got.Temperature)
	}
	if got.TopP == nil {
		t.Fatal("expected top_p=0 to be sent explicitly")
	}
	if *got.TopP != 0 {
		t.Fatalf("expected top_p=0, got %v", *got.TopP)
	}
	if !got.Stream {
		t.Fatal("expected stream=true")
	}
}

func TestOpenAICompatProviderStreamOmitsUnsetTemperatureAndTopP(t *testing.T) {
	var got struct {
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := NewOpenAICompatProvider(ts.URL, "test-key", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if ev.Type == EventDone {
			break
		}
	}

	if got.Temperature != nil {
		t.Fatalf("expected temperature to be omitted, got %v", *got.Temperature)
	}
	if got.TopP != nil {
		t.Fatalf("expected top_p to be omitted, got %v", *got.TopP)
	}
}
