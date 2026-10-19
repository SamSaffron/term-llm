package llm

import (
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestBuildResponsesAssistantItemsUsesOnlyOpaqueReplayWhenPresent(t *testing.T) {
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[]}`)
	items := buildResponsesAssistantItems([]core.Part{
		{Type: core.PartText, Text: "derived text"},
		{Type: core.PartToolActivity, ToolActivity: &core.ToolActivity{ID: "ws_1", Name: core.WebSearchToolName, Status: core.ToolActivityCompleted}},
		{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{}`)}},
		{Type: core.PartProviderReplay, ProviderReplay: &core.ProviderReplayItem{Raw: raw}},
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
