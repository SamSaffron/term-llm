package responses

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func replayPart(raw string) Part {
	return Part{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{Raw: json.RawMessage(raw)}}
}

const testWebSearchCallRaw = `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"example"}}`

func TestScanResponsesHistory(t *testing.T) {
	discovery := Message{Role: RoleAssistant, Parts: []Part{
		{Type: PartDiscoveryCall, DiscoveryCall: &ToolDiscoveryCall{ID: "call_1", Arguments: json.RawMessage(`{}`)}},
	}}
	tests := []struct {
		name     string
		messages []Message
		want     responsesHistoryFacts
	}{
		{name: "empty"},
		{
			name:     "plain text",
			messages: []Message{UserText("q"), AssistantText("a")},
		},
		{
			name: "replayed web search call",
			messages: []Message{UserText("q"), {Role: RoleAssistant, Parts: []Part{
				replayPart(`{"type":"reasoning","id":"rs_1","encrypted_content":"abc"}`),
				replayPart(testWebSearchCallRaw),
			}}},
			want: responsesHistoryFacts{HasReplayedWebSearch: true},
		},
		{
			name: "marker inside another item is not a web search call",
			messages: []Message{{Role: RoleAssistant, Parts: []Part{
				replayPart(`{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"the \"web_search_call\" item"}]}`),
			}}},
		},
		{
			name:     "web search text outside replay is ignored",
			messages: []Message{UserText(`{"type":"web_search_call"}`)},
		},
		{
			name:     "discovery parts",
			messages: []Message{UserText("q"), discovery},
			want:     responsesHistoryFacts{HasDiscoveryParts: true},
		},
		{
			name: "both",
			messages: []Message{discovery, {Role: RoleAssistant, Parts: []Part{
				replayPart(testWebSearchCallRaw),
			}}},
			want: responsesHistoryFacts{HasDiscoveryParts: true, HasReplayedWebSearch: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scanResponsesHistory(tt.messages); got != tt.want {
				t.Fatalf("scanResponsesHistory() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGuardReplayedWebSearch(t *testing.T) {
	fn := ResponsesTool{Type: "function", Name: "example_tool"}
	ptc := map[string]any{"type": "programmatic_tool_calling"}
	toolSearch := ResponsesToolSearchTool{Type: "tool_search", Execution: "client"}
	replayOnly := ResponsesWebSearchTool{Type: "web_search", ExternalWebAccess: boolPtr(false)}
	named := map[string]any{"type": "function", "name": "example_tool"}
	namedToolSearch := map[string]any{"type": "tool_search"}

	searched := []Message{
		UserText("q"),
		{Role: RoleAssistant, Parts: []Part{replayPart(testWebSearchCallRaw), {Type: PartText, Text: "answer"}}},
		UserText("again"),
	}
	stripped := []Message{
		UserText("q"),
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "answer"}}},
		UserText("again"),
	}

	tests := []struct {
		name         string
		messages     []Message
		tools        []any
		choice       any
		wantTools    []any
		wantChoice   any
		wantMessages []Message
	}{
		{
			name:         "no replayed search leaves request untouched",
			messages:     stripped,
			tools:        []any{fn},
			choice:       "auto",
			wantTools:    []any{fn},
			wantChoice:   "auto",
			wantMessages: stripped,
		},
		{
			name:         "declared web_search leaves request untouched",
			tools:        []any{ResponsesWebSearchTool{Type: "web_search"}, fn},
			wantTools:    []any{ResponsesWebSearchTool{Type: "web_search"}, fn},
			wantMessages: searched,
		},
		{
			name:         "declared web_search_preview leaves request untouched",
			tools:        []any{ResponsesWebSearchTool{Type: "web_search_preview"}},
			wantTools:    []any{ResponsesWebSearchTool{Type: "web_search_preview"}},
			wantMessages: searched,
		},
		{
			name:         "no caller tools forbids tool calls",
			wantTools:    []any{replayOnly},
			wantChoice:   "none",
			wantMessages: searched,
		},
		{
			name:         "required with no caller tools becomes none",
			choice:       "required",
			wantTools:    []any{replayOnly},
			wantChoice:   "none",
			wantMessages: searched,
		},
		{
			name:      "default choice restricts to caller tools",
			tools:     []any{fn, ptc},
			wantTools: []any{replayOnly, fn, ptc},
			wantChoice: map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []map[string]any{
				{"type": "function", "name": "example_tool"},
				{"type": "programmatic_tool_calling"},
			}},
			wantMessages: searched,
		},
		{
			name:      "required choice keeps required mode",
			tools:     []any{fn},
			choice:    "required",
			wantTools: []any{replayOnly, fn},
			wantChoice: map[string]any{"type": "allowed_tools", "mode": "required", "tools": []map[string]any{
				{"type": "function", "name": "example_tool"},
			}},
			wantMessages: searched,
		},
		{
			name:         "named choice is left alone",
			tools:        []any{fn},
			choice:       named,
			wantTools:    []any{replayOnly, fn},
			wantChoice:   named,
			wantMessages: searched,
		},
		{
			name:         "none choice is left alone",
			tools:        []any{fn},
			choice:       "none",
			wantTools:    []any{replayOnly, fn},
			wantChoice:   "none",
			wantMessages: searched,
		},
		{
			name:         "named tool_search choice is left alone",
			tools:        []any{fn, toolSearch},
			choice:       namedToolSearch,
			wantTools:    []any{replayOnly, fn, toolSearch},
			wantChoice:   namedToolSearch,
			wantMessages: searched,
		},
		{
			// allowed_tools rejects tool_search, so the replayed search is dropped
			// instead of declaring a tool the model could call.
			name:         "tool_search caller tool strips replayed search",
			tools:        []any{fn, toolSearch},
			wantTools:    []any{fn, toolSearch},
			wantMessages: stripped,
		},
		{
			name:         "tool_search with required strips replayed search",
			tools:        []any{toolSearch},
			choice:       "required",
			wantTools:    []any{toolSearch},
			wantChoice:   "required",
			wantMessages: stripped,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := tt.messages
			if messages == nil {
				messages = searched
			}
			callerTools := slices.Clone(tt.tools)
			before, _ := json.Marshal(messages)
			req := ResponsesRequest{Messages: messages, Tools: tt.tools, ToolChoice: tt.choice}
			guardReplayedWebSearch(&req, scanResponsesHistory(messages))
			if !reflect.DeepEqual(req.Tools, tt.wantTools) {
				t.Fatalf("tools = %#v, want %#v", req.Tools, tt.wantTools)
			}
			if !reflect.DeepEqual(req.ToolChoice, tt.wantChoice) {
				t.Fatalf("tool_choice = %#v, want %#v", req.ToolChoice, tt.wantChoice)
			}
			if !reflect.DeepEqual(req.Messages, tt.wantMessages) {
				t.Fatalf("messages = %#v, want %#v", req.Messages, tt.wantMessages)
			}
			if !reflect.DeepEqual(tt.tools, callerTools) {
				t.Fatalf("caller tool slice mutated: %#v", tt.tools)
			}
			if after, _ := json.Marshal(messages); string(after) != string(before) {
				t.Fatalf("caller messages mutated")
			}
		})
	}
}

func TestGuardReplayedWebSearchStripsPrebuiltInput(t *testing.T) {
	req := ResponsesRequest{
		Input: []ResponsesInputItem{
			{Type: "message", Role: "user", Content: "q"},
			{Raw: json.RawMessage(testWebSearchCallRaw)},
			{Raw: json.RawMessage(`{"type":"reasoning","id":"rs_1"}`)},
		},
		Tools: []any{ResponsesToolSearchTool{Type: "tool_search"}},
	}
	guardReplayedWebSearch(&req, responsesHistoryFacts{HasReplayedWebSearch: true})
	if len(req.Input) != 2 || req.Input[0].Type != "message" || string(req.Input[1].Raw) != `{"type":"reasoning","id":"rs_1"}` {
		t.Fatalf("input = %#v, want web_search_call removed", req.Input)
	}
}

func BenchmarkScanResponsesHistoryWithoutWebSearch(b *testing.B) {
	blob := strings.Repeat("x", 8000)
	var messages []Message
	for i := 0; i < 500; i++ {
		messages = append(messages, UserText("q"), Message{Role: RoleAssistant, Parts: []Part{
			replayPart(`{"type":"reasoning","id":"rs_1","encrypted_content":"` + blob + `"}`),
			{Type: PartText, Text: "answer"},
		}})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if scanResponsesHistory(messages).HasReplayedWebSearch {
			b.Fatal("unexpected web search")
		}
	}
}
