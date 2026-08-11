package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestToolDiscoveryReplayPartsRoundTripForSessionResume(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "chatgpt", ProviderKey: "chatgpt", Model: "gpt-5.6-luna-medium"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	message := NewMessage(sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartDiscoveryCall, DiscoveryCall: &llm.ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"eta"}`)}},
		{Type: llm.PartDiscoveryOutput, DiscoveryOutput: &llm.ToolDiscoveryOutput{
			CallID: "search-1", CatalogueHash: "catalogue", CatalogueGen: 7,
			Tools: []llm.DiscoveredTool{{Spec: llm.ToolSpec{Name: "federation__eta", Description: "ETA", Schema: map[string]any{"type": "object"}}, SchemaHash: "schema"}},
		}},
	}}, 0)
	if err := store.AddMessage(ctx, sess.ID, message); err != nil {
		t.Fatal(err)
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Parts) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	call := messages[0].Parts[0].DiscoveryCall
	output := messages[0].Parts[1].DiscoveryOutput
	if call == nil || call.ID != "search-1" || string(call.Arguments) != `{"query":"eta"}` {
		t.Fatalf("call = %#v", call)
	}
	if output == nil || output.CatalogueGen != 7 || len(output.Tools) != 1 || output.Tools[0].SchemaHash != "schema" || output.Tools[0].Spec.Name != "federation__eta" {
		t.Fatalf("output = %#v", output)
	}
	if messages[0].TextContent != "" {
		t.Fatalf("discovery replay leaked into visible text: %q", messages[0].TextContent)
	}
}
