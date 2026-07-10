package llm

import (
	"encoding/json"
	"testing"
)

func TestAttachProviderReplayPartsDeepCopiesOpaqueState(t *testing.T) {
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"secret"}`)
	msg := Message{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "visible"}}}
	got := attachProviderReplayParts(msg, []Part{{
		Type: PartProviderReplay,
		ProviderReplay: &ProviderReplayItem{
			Raw: raw,
		},
	}})
	if len(got.Parts) != 2 || got.Parts[1].Type != PartProviderReplay || got.Parts[1].ProviderReplay == nil {
		t.Fatalf("message parts = %#v", got.Parts)
	}
	raw[0] = '['
	if got.Parts[1].ProviderReplay.Raw[0] != '{' {
		t.Fatalf("replay raw aliases source: %q", got.Parts[1].ProviderReplay.Raw)
	}
}

func TestBuildResponsesAssistantItemsUsesOnlyOpaqueReplayWhenPresent(t *testing.T) {
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[]}`)
	items := buildResponsesAssistantItems([]Part{
		{Type: PartText, Text: "derived text"},
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
