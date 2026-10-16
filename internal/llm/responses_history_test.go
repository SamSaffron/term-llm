package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
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

// TestResponsesProvidersGuardReplayedWebSearchOnTheWire checks the request each
// Responses provider actually sends, so provider wiring cannot drift from the
// shared guard.
func TestResponsesProvidersGuardReplayedWebSearchOnTheWire(t *testing.T) {
	history := []Message{
		UserText("q"),
		{Role: RoleAssistant, Parts: []Part{replayPart(testWebSearchCallRaw), {Type: PartText, Text: "answer"}}},
		UserText("again"),
	}
	fnTool := ToolSpec{Name: "example_tool", Description: "example", Schema: map[string]any{"type": "object"}}

	type wire struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice json.RawMessage  `json:"tool_choice"`
	}
	sse := strings.Join([]string{
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	providers := map[string]struct {
		searchType string
		stream     func(t *testing.T, req Request) wire
	}{
		"chatgpt": {searchType: "web_search", stream: func(t *testing.T, req Request) wire {
			origClient := chatGPTHTTPClient
			defer func() { chatGPTHTTPClient = origClient }()
			var got wire
			chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: make(http.Header)}, nil
			})}
			provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
				AccessToken: "test-token", AccountID: "test-account", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			}, "gpt-5.5-medium")
			stream, err := provider.Stream(context.Background(), req)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer stream.Close()
			drainStreamToDone(t, stream)
			return got
		}},
		"openai": {searchType: "web_search_preview", stream: func(t *testing.T, req Request) wire {
			var got wire
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(sse))
			}))
			defer ts.Close()
			provider := &OpenAIProvider{apiKey: "test-key", model: "gpt-4.1", responsesClient: &ResponsesClient{
				BaseURL: ts.URL, GetAuthHeader: func() string { return "Bearer test-key" }, HTTPClient: ts.Client(),
			}}
			stream, err := provider.Stream(context.Background(), req)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer stream.Close()
			drainStreamToDone(t, stream)
			return got
		}},
	}

	for name, p := range providers {
		t.Run(name, func(t *testing.T) {
			cases := []struct {
				name          string
				req           Request
				wantToolsJSON string   // exact tools array, when set
				wantToolTypes []string // tool types in order, when set
				wantChoice    string
			}{
				{
					name:          "replay without tools",
					req:           Request{Messages: history},
					wantToolsJSON: `[{"external_web_access":false,"type":"web_search"}]`,
					wantChoice:    `"none"`,
				},
				{
					name:          "replay with caller tool",
					req:           Request{Messages: history, Tools: []ToolSpec{fnTool}},
					wantToolTypes: []string{"web_search", "function"},
					wantChoice:    `{"mode":"auto","tools":[{"name":"example_tool","type":"function"}],"type":"allowed_tools"}`,
				},
				{
					name:          "replay with required choice",
					req:           Request{Messages: history, Tools: []ToolSpec{fnTool}, ToolChoice: ToolChoice{Mode: ToolChoiceRequired}},
					wantToolTypes: []string{"web_search", "function"},
					wantChoice:    `{"mode":"required","tools":[{"name":"example_tool","type":"function"}],"type":"allowed_tools"}`,
				},
				{
					name:          "replay with none choice",
					req:           Request{Messages: history, Tools: []ToolSpec{fnTool}, ToolChoice: ToolChoice{Mode: ToolChoiceNone}},
					wantToolTypes: []string{"web_search", "function"},
					wantChoice:    `"none"`,
				},
				{
					name:          "search enabled is unchanged",
					req:           Request{Messages: history, Search: true},
					wantToolsJSON: `[{"type":"` + p.searchType + `"}]`,
				},
				{
					name: "no replay is unchanged",
					req:  Request{Messages: []Message{UserText("q")}},
				},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					got := p.stream(t, c.req)
					if c.wantToolTypes != nil {
						var types []string
						for _, tool := range got.Tools {
							toolType, _ := tool["type"].(string)
							types = append(types, toolType)
						}
						if !slices.Equal(types, c.wantToolTypes) {
							t.Fatalf("tool types = %v, want %v", types, c.wantToolTypes)
						}
					} else {
						var gotJSON string
						if got.Tools != nil {
							encoded, _ := json.Marshal(got.Tools)
							gotJSON = string(encoded)
						}
						if gotJSON != c.wantToolsJSON {
							t.Fatalf("tools = %s, want %s", gotJSON, c.wantToolsJSON)
						}
					}
					if string(got.ToolChoice) != c.wantChoice {
						t.Fatalf("tool_choice = %s, want %s", got.ToolChoice, c.wantChoice)
					}
				})
			}
		})
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
