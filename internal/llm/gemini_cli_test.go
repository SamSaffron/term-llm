package llm

import "testing"

func TestBuildGeminiCLIContents_DeveloperRole(t *testing.T) {
	_, contents := buildGeminiCLIContents([]Message{
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "preserve this policy"}}},
		UserText("create the briefing"),
	})

	if len(contents) != 1 || contents[0]["role"] != "user" {
		t.Fatalf("contents = %#v, want one user message", contents)
	}
	parts, ok := contents[0]["parts"].([]map[string]interface{})
	if !ok || len(parts) != 1 {
		t.Fatalf("user parts = %#v, want one text part", contents[0]["parts"])
	}
	got, _ := parts[0]["text"].(string)
	want := "<developer>\npreserve this policy\n</developer>\n\ncreate the briefing"
	if got != want {
		t.Fatalf("user text = %q, want %q", got, want)
	}
}

func TestBuildGeminiCLIContents_DropsDanglingToolCalls(t *testing.T) {
	_, contents := buildGeminiCLIContents([]Message{
		UserText("Run shell"),
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartText, Text: "Working"},
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
		UserText("new request"),
	})

	if len(contents) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(contents))
	}

	modelContent := contents[1]
	role, _ := modelContent["role"].(string)
	if role != "model" {
		t.Fatalf("expected role model, got %q", role)
	}

	parts, ok := modelContent["parts"].([]map[string]interface{})
	if !ok {
		t.Fatalf("expected model parts []map[string]interface{}, got %T", modelContent["parts"])
	}

	var sawText bool
	for _, part := range parts {
		if _, hasFunctionCall := part["functionCall"]; hasFunctionCall {
			t.Fatalf("expected dangling functionCall to be removed, got %#v", part["functionCall"])
		}
		if text, ok := part["text"].(string); ok && text == "Working" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("expected assistant text to be preserved, got %#v", parts)
	}
}

func TestCollectGeminiCLITextParts_ConcatenatesNonThoughtTextParts(t *testing.T) {
	got := collectGeminiCLITextParts([]geminiCLITextPart{
		{Text: "first"},
		{Text: " hidden", Thought: true},
		{Text: " second"},
		{Text: ""},
		{Text: " third"},
	})

	if got != "first second third" {
		t.Fatalf("collectGeminiCLITextParts() = %q, want %q", got, "first second third")
	}
}
