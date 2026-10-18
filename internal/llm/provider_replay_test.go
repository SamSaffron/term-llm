package llm

import (
	"encoding/json"
	"testing"
)

func TestBuildResponsesAssistantItemsUsesOnlyOpaqueReplayWhenPresent(t *testing.T) {
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[]}`)
	items := buildResponsesAssistantItems([]Part{
		{Type: PartText, Text: "derived text"},
		{Type: PartToolActivity, ToolActivity: &ToolActivity{ID: "ws_1", Name: WebSearchToolName, Status: ToolActivityCompleted}},
		{Type: PartToolCall, ToolCall: &ToolCall{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{}`)}},
		{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{Raw: raw}},
	})
	if len(items) != 1 {
		t.Fatalf("items = %#v, want only opaque replay item", items)
	}
	encoded, err := json.Marshal(items[0])
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(encoded) != string(raw) {
		t.Fatalf("encoded replay = %s, want exact %s", encoded, raw)
	}
}
