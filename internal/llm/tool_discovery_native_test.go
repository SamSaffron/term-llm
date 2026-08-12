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

func TestChatGPTDiscoveryWireTranslationCoalescesNamespaceChildrenAndPreservesOutputSchema(t *testing.T) {
	call := &ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"shipping ETA"}`)}
	namespace := func(child string) *ToolNamespaceIdentity {
		return &ToolNamespaceIdentity{Name: "federation", ChildName: child, Description: "Federated logistics tools."}
	}
	output := &ToolDiscoveryOutput{CallID: "search-1", Tools: []DiscoveredTool{
		{
			Spec: ToolSpec{
				Name:         "federation__shipping_eta",
				Description:  "Find ETA",
				Schema:       map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"eta": map[string]any{"type": "string"}}},
				Namespace:    namespace("shipping_eta"),
			},
			SchemaHash: "hash-1",
		},
		{
			Spec: ToolSpec{
				Name:        "federation__track_shipment",
				Description: "Track shipment",
				Schema:      map[string]any{"type": "object"},
				Namespace:   namespace("track_shipment"),
			},
			SchemaHash: "hash-2",
		},
	}}
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
		t.Fatalf("output wire = %#v, want one coalesced namespace", outputWire)
	}
	group, _ := tools[0].(map[string]any)
	if group["type"] != "namespace" || group["name"] != "federation" || group["description"] != "Federated logistics tools." {
		t.Fatalf("namespace wire = %#v", group)
	}
	children, _ := group["tools"].([]any)
	if len(children) != 2 {
		t.Fatalf("namespace children = %#v", children)
	}
	first, _ := children[0].(map[string]any)
	second, _ := children[1].(map[string]any)
	if first["defer_loading"] != true || first["name"] != "shipping_eta" || second["name"] != "track_shipment" {
		t.Fatalf("loaded children = %#v", children)
	}
	if _, ok := first["output_schema"].(map[string]any); !ok {
		t.Fatalf("namespace child lost output_schema: %#v", first)
	}
	ordinary := BuildResponsesTools([]ToolSpec{output.Tools[0].Spec})
	raw, _ := json.Marshal(ordinary[0])
	var ordinaryWire map[string]any
	_ = json.Unmarshal(raw, &ordinaryWire)
	if ordinaryWire["name"] != "federation__shipping_eta" {
		t.Fatalf("portable flattened name changed: %#v", ordinaryWire)
	}
	if _, exists := ordinaryWire["defer_loading"]; exists {
		t.Fatalf("ordinary top-level tool gained defer_loading: %#v", ordinaryWire)
	}
}

func TestChatGPTDiscoveryWireKeepsSameChildNameInSeparateNamespaces(t *testing.T) {
	output := ToolDiscoveryOutput{CallID: "search", Tools: []DiscoveredTool{
		{Spec: ToolSpec{Name: "alpha__lookup", Schema: map[string]any{}, Namespace: &ToolNamespaceIdentity{Name: "alpha", ChildName: "lookup"}}},
		{Spec: ToolSpec{Name: "beta__lookup", Schema: map[string]any{}, Namespace: &ToolNamespaceIdentity{Name: "beta", ChildName: "lookup"}}},
	}}
	raw, err := buildResponsesDiscoveryOutputItem(output)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Tools []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 2 || wire.Tools[0].Name != "alpha" || wire.Tools[1].Name != "beta" || wire.Tools[0].Tools[0].Name != "lookup" || wire.Tools[1].Tools[0].Name != "lookup" {
		t.Fatalf("namespace output = %s", raw)
	}
}

func TestNamespaceMetadataDoesNotChangePortableFunctionName(t *testing.T) {
	spec := ToolSpec{
		Name:      "federation__lookup",
		Schema:    map[string]any{"type": "object"},
		Namespace: &ToolNamespaceIdentity{Name: "federation", ChildName: "lookup"},
	}
	compat, err := buildCompatTools([]ToolSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	if len(compat) != 1 || compat[0].Function.Name != "federation__lookup" {
		t.Fatalf("portable tools = %#v", compat)
	}
	responses := BuildResponsesTools([]ToolSpec{spec})
	if len(responses) != 1 || responses[0].(ResponsesTool).Name != "federation__lookup" {
		t.Fatalf("ordinary Responses tools = %#v", responses)
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

func TestResponsesEventHandlerPreservesNativeNamespaceFunctionCallIdentity(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 3)
	send := eventSender{ctx: context.Background(), ch: events}
	data := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call-1","namespace":"federation","name":"shipping_eta","arguments":"{\"order\":\"42\"}"}}`)
	if _, err := handler.HandleJSONEvent(data, "response.output_item.done", send); err != nil {
		t.Fatal(err)
	}
	if err := handler.Finish(send); err != nil {
		t.Fatal(err)
	}
	event := <-events
	if event.Type != EventToolCall || event.Tool == nil {
		t.Fatalf("event = %#v", event)
	}
	if event.Tool.Name != "" || event.Tool.Namespace != "federation" || event.Tool.ChildName != "shipping_eta" {
		t.Fatalf("native call identity = %#v", event.Tool)
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
	output := &ToolDiscoveryOutput{CallID: "search-1", Tools: []DiscoveredTool{{Spec: ToolSpec{Name: "federation__eta", Schema: map[string]any{"type": "object"}, Namespace: &ToolNamespaceIdentity{Name: "federation", ChildName: "eta", Description: "Federation tools."}}, SchemaHash: "h"}}}
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
	var foundCall, foundOutput, foundNamespace bool
	for _, item := range items {
		if len(item.Raw) == 0 {
			continue
		}
		var ref struct {
			Type  string `json:"type"`
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(item.Raw, &ref)
		foundCall = foundCall || ref.Type == "tool_search_call"
		foundOutput = foundOutput || ref.Type == "tool_search_output"
		if ref.Type == "tool_search_output" && len(ref.Tools) == 1 && ref.Tools[0].Type == "namespace" && ref.Tools[0].Name == "federation" {
			foundNamespace = true
		}
	}
	if !foundCall || !foundOutput || !foundNamespace {
		t.Fatalf("full replay call=%v output=%v namespace=%v items=%#v", foundCall, foundOutput, foundNamespace, items)
	}
}
