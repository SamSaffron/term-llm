package engine

import (
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestRestoreToolDiscoveryReplaySurvivesCompactionProjection(t *testing.T) {
	call := &core.ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"eta"}`)}
	output := &core.ToolDiscoveryOutput{CallID: "search-1", Tools: []core.DiscoveredTool{{Spec: core.ToolSpec{Name: "federation__eta", Schema: map[string]any{"type": "object"}, Namespace: &core.ToolNamespaceIdentity{Name: "federation", ChildName: "eta", Description: "Federation tools."}}, SchemaHash: "h"}}}
	original := []core.Message{
		core.UserText("old"),
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartDiscoveryCall, DiscoveryCall: call}, {Type: core.PartDiscoveryOutput, DiscoveryOutput: output}}},
		core.AssistantText("old answer"),
	}
	replay := collectToolDiscoveryReplay(original)
	active := restoreToolDiscoveryReplay([]core.Message{core.SystemText("system"), core.UserText("summary"), core.UserText("latest")}, replay)
	if got := len(collectToolDiscoveryReplay(active)); got != 2 {
		t.Fatalf("restored discovery parts = %d, want 2", got)
	}
}
