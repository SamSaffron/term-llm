package responsesproviders

import (
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	"reflect"
	"testing"
)

func TestChatGPTDiscoveryWireTranslationCoalescesNamespaceChildren(t *testing.T) {
	call := &ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"shipping ETA"}`)}
	namespace := func(child string) *core.ToolNamespaceIdentity {
		return &core.ToolNamespaceIdentity{Name: "federation", ChildName: child, Description: "Federated logistics tools."}
	}
	output := &ToolDiscoveryOutput{CallID: "search-1", Tools: []core.DiscoveredTool{
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
	items := responses.BuildResponsesInput(messages)
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
	if _, ok := first["output_schema"]; ok {
		t.Fatalf("namespace child advertised unsupported output_schema: %#v", first)
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
	output := ToolDiscoveryOutput{CallID: "search", Tools: []core.DiscoveredTool{
		{Spec: ToolSpec{Name: "alpha__lookup", Schema: map[string]any{}, Namespace: &core.ToolNamespaceIdentity{Name: "alpha", ChildName: "lookup"}}},
		{Spec: ToolSpec{Name: "beta__lookup", Schema: map[string]any{}, Namespace: &core.ToolNamespaceIdentity{Name: "beta", ChildName: "lookup"}}},
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

func TestChatGPTToolsAndChoiceForcesNativeToolSearch(t *testing.T) {
	fn := responses.ResponsesTool{Type: "function", Name: "example_tool"}
	search := ResponsesToolSearchTool{Type: "tool_search", Execution: "client"}
	tests := []struct {
		name       string
		tools      []any
		choice     ToolChoice
		toolSearch *ResponsesToolSearchTool
		wantTools  []any
		wantChoice any
	}{
		{
			// {"type":"tool_search"} is rejected by the backend; tool_search alone
			// with "required" forces it exactly.
			name:       "forced native tool_search",
			tools:      []any{fn, search},
			choice:     ToolChoice{Mode: ToolChoiceName, Name: "tool_search"},
			toolSearch: &search,
			wantTools:  []any{search},
			wantChoice: "required",
		},
		{
			name:       "named tool_search without native discovery is a function choice",
			tools:      []any{responses.ResponsesTool{Type: "function", Name: "tool_search"}},
			choice:     ToolChoice{Mode: ToolChoiceName, Name: "tool_search"},
			wantTools:  []any{responses.ResponsesTool{Type: "function", Name: "tool_search"}},
			wantChoice: map[string]any{"type": "function", "name": "tool_search"},
		},
		{
			name:       "other named tool is unchanged",
			tools:      []any{fn, search},
			choice:     ToolChoice{Mode: ToolChoiceName, Name: "example_tool"},
			toolSearch: &search,
			wantTools:  []any{fn, search},
			wantChoice: map[string]any{"type": "function", "name": "example_tool"},
		},
		{
			name:       "auto is unchanged",
			tools:      []any{fn, search},
			choice:     ToolChoice{Mode: ToolChoiceAuto},
			toolSearch: &search,
			wantTools:  []any{fn, search},
			wantChoice: "auto",
		},
		{
			name:       "unset choice stays unset",
			tools:      []any{fn},
			wantTools:  []any{fn},
			wantChoice: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools, choice := chatGPTToolsAndChoice(tt.tools, tt.choice, tt.toolSearch)
			if !reflect.DeepEqual(tools, tt.wantTools) {
				t.Fatalf("tools = %#v, want %#v", tools, tt.wantTools)
			}
			if !reflect.DeepEqual(choice, tt.wantChoice) {
				t.Fatalf("tool_choice = %#v, want %#v", choice, tt.wantChoice)
			}
		})
	}
}

// TestChatGPTForcedToolSearchWithReplayedWebSearch pins the order used by
// ChatGPTProvider.Stream: forcing tool_search narrows the tools first, then the
// replay guard strips the replayed search instead of declaring a callable one.
func TestChatGPTForcedToolSearchWithReplayedWebSearch(t *testing.T) {
	search := ResponsesToolSearchTool{Type: "tool_search", Execution: "client"}
	history := []Message{
		UserText("q"),
		{Role: RoleAssistant, Parts: []Part{
			{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{Raw: json.RawMessage(`{"type":"web_search_call","id":"ws_1","status":"completed"}`)}},
			{Type: PartText, Text: "answer"},
		}},
		UserText("find a tool"),
	}
	req := ResponsesRequest{Messages: history}
	// Search on: web_search would be declared, but the forced turn withholds it.
	tools := []any{ResponsesWebSearchTool{Type: "web_search"}, responses.ResponsesTool{Type: "function", Name: "example_tool"}, search}
	req.Tools, req.ToolChoice = chatGPTToolsAndChoice(tools, ToolChoice{Mode: ToolChoiceName, Name: "tool_search"}, &search)
	guardReplayedWebSearch(&req, scanResponsesHistory(history))

	if !reflect.DeepEqual(req.Tools, []any{search}) || req.ToolChoice != "required" {
		t.Fatalf("tools=%#v tool_choice=%#v, want only tool_search with required", req.Tools, req.ToolChoice)
	}
	for _, item := range responses.BuildResponsesInput(req.Messages) {
		if len(item.Raw) > 0 && responses.IsWebSearchCall(item.Raw) {
			t.Fatalf("input still replays web_search_call: %s", item.Raw)
		}
	}
	if !scanResponsesHistory(history).HasReplayedWebSearch {
		t.Fatal("caller history was mutated")
	}
}
func TestDiscoveryContinuationSendsOutputWithoutReplayingCall(t *testing.T) {
	callRaw, _ := buildResponsesDiscoveryCallItem(ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"eta"}`)})
	outputRaw, _ := buildResponsesDiscoveryOutputItem(ToolDiscoveryOutput{CallID: "search-1", Tools: []core.DiscoveredTool{{Spec: ToolSpec{Name: "eta", Schema: map[string]any{"type": "object"}}, SchemaHash: "h"}}})
	items := []ResponsesInputItem{
		{Type: "message", Role: "user", Content: "find eta"},
		{Raw: callRaw},
		{Raw: outputRaw},
	}
	got := responses.TrimContinuationToDiscoveryOutput(items)
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

func TestChatGPTTranslatesRestoredToolDiscoveryReplay(t *testing.T) {
	call := &core.ToolDiscoveryCall{ID: "search-1", Arguments: json.RawMessage(`{"query":"eta"}`)}
	output := &core.ToolDiscoveryOutput{CallID: "search-1", Tools: []core.DiscoveredTool{{Spec: core.ToolSpec{Name: "federation__eta", Schema: map[string]any{"type": "object"}, Namespace: &core.ToolNamespaceIdentity{Name: "federation", ChildName: "eta", Description: "Federation tools."}}, SchemaHash: "h"}}}
	original := []Message{
		UserText("summary"),
		{Role: RoleAssistant, Parts: []Part{{Type: PartDiscoveryCall, DiscoveryCall: call}, {Type: PartDiscoveryOutput, DiscoveryOutput: output}}},
	}
	// Engine restoration is covered in the engine package; this checks the
	// restored shape (replay inserted ahead of the latest turn) translates.
	active := []Message{original[0], original[1], UserText("latest")}
	translated, err := translateChatGPTDiscoveryMessages(active)
	if err != nil {
		t.Fatal(err)
	}
	items := responses.BuildResponsesInput(translated)
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

func TestResponsesContentOnlyToolOmitsOutputSchema(t *testing.T) {
	spec := core.ToolSpec{
		Name:   "cua-driver__list_windows",
		Schema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"windows": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		}},
	}
	// MCP content is human-readable, not necessarily JSON matching its output schema.
	input := responses.BuildResponsesInput([]Message{
		{Role: RoleAssistant, Parts: []Part{{Type: core.PartToolCall, ToolCall: &ToolCall{
			ID: "call_windows", Name: spec.Name, Arguments: json.RawMessage(`{}`),
		}}}},
		{Role: core.RoleTool, Parts: []Part{{
			Type: core.PartToolResult, ToolResult: &core.ToolResult{
				ID: "call_windows", Name: spec.Name, Content: "Found 0 window(s).",
				ContentParts: []core.ToolContentPart{{Type: core.ToolContentPartText, Text: "Found 0 window(s)."}},
			},
		}}}})
	if len(input) != 2 || input[1].Output != "Found 0 window(s)." {
		t.Fatalf("tool result = %#v", input)
	}
	for _, mode := range []string{"ordinary", "programmatic", "discovered", "namespace"} {
		t.Run(mode, func(t *testing.T) {
			selected := spec
			var wire any
			switch mode {
			case "ordinary":
				wire = responses.BuildResponsesTools([]core.ToolSpec{selected})
			case "programmatic":
				wire = responses.BuildResponsesToolsWithOptions([]core.ToolSpec{selected}, core.ProgrammaticToolCallingOptions{Enabled: true})
			default:
				if mode == "namespace" {
					selected.Namespace = &core.ToolNamespaceIdentity{Name: "cua-driver", ChildName: "list_windows"}
				}
				raw, err := buildResponsesDiscoveryOutputItem(core.ToolDiscoveryOutput{
					CallID: "search", Tools: []core.DiscoveredTool{{Spec: selected}},
				})
				if err != nil {
					t.Fatal(err)
				}
				wire = raw
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			var check func(any)
			check = func(value any) {
				switch value := value.(type) {
				case map[string]any:
					if _, exists := value["output_schema"]; exists {
						t.Fatalf("content-only result cannot fulfill output_schema: %s", raw)
					}
					for _, child := range value {
						check(child)
					}
				case []any:
					for _, child := range value {
						check(child)
					}
				}
			}
			check(decoded)
			if selected.OutputSchema == nil {
				t.Fatal("provider serialization removed catalogue output schema")
			}
		})
	}
}
