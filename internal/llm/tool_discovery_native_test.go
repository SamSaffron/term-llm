package llm

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestChatGPTNativeToolDiscoveryCapabilityIsExact(t *testing.T) {
	creds := &credentials.ChatGPTCredentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	for _, tc := range []struct {
		name      string
		model     string
		websocket bool
		want      bool
	}{
		{name: "luna websocket", model: "gpt-5.6-luna-medium", websocket: true, want: true},
		{name: "luna HTTP", model: "gpt-5.6-luna-medium", websocket: false},
		{name: "sol websocket", model: "gpt-5.6-sol-medium", websocket: true},
		{name: "older websocket", model: "gpt-5.5-medium", websocket: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := NewChatGPTProviderWithCredsAndOptions(creds, tc.model, ChatGPTProviderOptions{UseWebSocket: tc.websocket})
			if got := provider.NativeToolDiscoverySupport(tc.model).Supported; got != tc.want {
				t.Fatalf("supported = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChatGPTDiscoveryWireTranslationDefersOnlyOutputSchemas(t *testing.T) {
	call := &ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"shipping ETA"}`)}
	output := &ToolDiscoveryOutput{CallID: "search-1", Tools: []DiscoveredTool{{
		Spec:       ToolSpec{Name: "federation__shipping_eta", Description: "Find ETA", Schema: map[string]any{"type": "object"}},
		SchemaHash: "hash-1",
	}}}
	messages, err := translateChatGPTDiscoveryMessages([]Message{{Role: RoleAssistant, Parts: []Part{
		{Type: PartDiscoveryCall, DiscoveryCall: call},
		{Type: PartDiscoveryOutput, DiscoveryOutput: output},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	items := BuildResponsesInput(messages)
	if len(items) != 2 {
		t.Fatalf("input items = %#v", items)
	}
	var callWire, outputWire map[string]any
	if err := json.Unmarshal(items[0].Raw, &callWire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(items[1].Raw, &outputWire); err != nil {
		t.Fatal(err)
	}
	if callWire["type"] != "tool_search_call" || callWire["execution"] != "client" || callWire["call_id"] != "search-1" {
		t.Fatalf("call wire = %#v", callWire)
	}
	tools, _ := outputWire["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("output wire = %#v", outputWire)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["defer_loading"] != true || tool["name"] != "federation__shipping_eta" {
		t.Fatalf("loaded tool wire = %#v", tool)
	}
	ordinary := BuildResponsesTools([]ToolSpec{output.Tools[0].Spec})
	raw, _ := json.Marshal(ordinary[0])
	if json.Valid(raw) && string(raw) == "" {
		t.Fatal("ordinary tool unexpectedly empty")
	}
	var ordinaryWire map[string]any
	_ = json.Unmarshal(raw, &ordinaryWire)
	if _, exists := ordinaryWire["defer_loading"]; exists {
		t.Fatalf("ordinary top-level tool gained defer_loading: %#v", ordinaryWire)
	}
}

func TestResponsesEventHandlerEmitsNeutralDiscoveryCall(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 2)
	send := eventSender{ctx: context.Background(), ch: events}
	data := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"tool_search_call","execution":"client","call_id":"search-1","status":"completed","arguments":{"tool_names":["federation__shipping_eta"]}}}`)
	if _, err := handler.HandleJSONEvent(data, "response.output_item.done", send); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Type != EventDiscoveryCall || event.DiscoveryCall == nil {
			t.Fatalf("event = %#v", event)
		}
		if string(event.DiscoveryCall.Arguments) != `{"tool_names":["federation__shipping_eta"]}` {
			t.Fatalf("arguments = %s", event.DiscoveryCall.Arguments)
		}
	default:
		t.Fatal("missing discovery call event")
	}
	if len(handler.replayItems) != 0 {
		t.Fatalf("native discovery leaked into opaque provider replay: %#v", handler.replayItems)
	}
}

func TestDiscoveryContinuationSendsOutputWithoutReplayingCall(t *testing.T) {
	callRaw, _ := buildResponsesDiscoveryCallItem(ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"eta"}`)})
	outputRaw, _ := buildResponsesDiscoveryOutputItem(ToolDiscoveryOutput{CallID: "search-1", Tools: []DiscoveredTool{{Spec: ToolSpec{Name: "eta", Schema: map[string]any{"type": "object"}}, SchemaHash: "h"}}})
	items := []ResponsesInputItem{
		{Type: "message", Role: "user", Content: "find eta"},
		{Raw: callRaw},
		{Raw: outputRaw},
	}
	got := trimResponsesContinuationToDiscoveryOutput(items)
	if len(got) != 1 {
		t.Fatalf("continuation items = %#v", got)
	}
	var ref struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(got[0].Raw, &ref); err != nil || ref.Type != "tool_search_output" {
		t.Fatalf("continuation item = %s, error=%v", got[0].Raw, err)
	}
}

func TestRestoreToolDiscoveryReplaySurvivesCompactionProjection(t *testing.T) {
	call := &ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"eta"}`)}
	output := &ToolDiscoveryOutput{CallID: "search-1", Tools: []DiscoveredTool{{Spec: ToolSpec{Name: "eta", Schema: map[string]any{"type": "object"}}, SchemaHash: "h"}}}
	original := []Message{
		UserText("old"),
		{Role: RoleAssistant, Parts: []Part{{Type: PartDiscoveryCall, DiscoveryCall: call}, {Type: PartDiscoveryOutput, DiscoveryOutput: output}}},
		AssistantText("old answer"),
	}
	replay := collectToolDiscoveryReplay(original)
	active := restoreToolDiscoveryReplay([]Message{SystemText("system"), UserText("summary"), UserText("latest")}, replay)
	if got := len(collectToolDiscoveryReplay(active)); got != 2 {
		t.Fatalf("restored discovery parts = %d, want 2", got)
	}
	translated, err := translateChatGPTDiscoveryMessages(active)
	if err != nil {
		t.Fatal(err)
	}
	items := BuildResponsesInput(translated)
	var foundCall, foundOutput bool
	for _, item := range items {
		if len(item.Raw) == 0 {
			continue
		}
		var ref struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(item.Raw, &ref)
		foundCall = foundCall || ref.Type == "tool_search_call"
		foundOutput = foundOutput || ref.Type == "tool_search_output"
	}
	if !foundCall || !foundOutput {
		t.Fatalf("full replay call=%v output=%v items=%#v", foundCall, foundOutput, items)
	}
}
