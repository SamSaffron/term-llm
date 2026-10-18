package responsesproviders_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
	. "github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
)

const testWebSearchCallRaw = `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"example"}}`

func replayPart(raw string) core.Part {
	return core.Part{Type: core.PartProviderReplay, ProviderReplay: &core.ProviderReplayItem{Raw: json.RawMessage(raw)}}
}

type capturedResponsesRequest struct {
	PreviousResponseID string                                 `json:"previous_response_id"`
	Input              []responses.ResponsesInputItem         `json:"input"`
	Reasoning          *responses.ResponsesReasoning          `json:"reasoning"`
	MultiAgent         *responses.ResponsesMultiAgent         `json:"multi_agent"`
	PromptCacheOptions *responses.ResponsesPromptCacheOptions `json:"prompt_cache_options"`
}

func newIsolatedResponsesProvider(t *testing.T, responseText, responseID string) (core.Provider, *rp.OpenAIProvider, <-chan capturedResponsesRequest) {
	t.Helper()

	captured := make(chan capturedResponsesRequest, 1)

	sse := strings.Join([]string{
		"event: response.output_text.delta",
		fmt.Sprintf(`data: {"delta":%q}`, responseText),
		"",
		"event: response.completed",
		fmt.Sprintf(`data: {"response":{"id":%q}}`, responseID),
		"",
		"data: [DONE]",
		"",
	}, "\n")

	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var payload capturedResponsesRequest
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return nil, err
			}
			captured <- payload

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(strings.NewReader(sse)),
			}, nil
		}),
	}

	provider := rp.NewOpenAIProviderForTest("test-key", "gpt-5.2", &responses.ResponsesClient{
		BaseURL:        "https://example.test/v1/responses",
		GetAuthHeader:  func() string { return "Bearer test-key" },
		HTTPClient:     httpClient,
		LastResponseID: "resp_live",
	})

	return WrapWithRetry(provider, DefaultRetryConfig()), provider, captured
}

func TestCompactUsesIsolatedProviderConversationState(t *testing.T) {
	provider, liveProvider, captured := newIsolatedResponsesProvider(t, "summary", "resp_compact")

	result, err := Compact(context.Background(), provider, "gpt-5.2", "sys", []core.Message{
		core.UserText("hello"),
		core.AssistantText("hi"),
		core.UserText("please summarize"),
	}, engine.DefaultCompactionConfig())
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if !strings.Contains(result.Summary, "<PREVIOUS_TURNS>") || !strings.Contains(result.Summary, "<SUMMARY_AND_NEXT_ACTIONS>") || !strings.Contains(result.Summary, "summary") {
		t.Fatalf("summary missing structured compaction content:\n%s", result.Summary)
	}

	if rp.OpenAIResponsesClientForTest(liveProvider).LastResponseID != "resp_live" {
		t.Fatalf("live LastResponseID = %q, want %q", rp.OpenAIResponsesClientForTest(liveProvider).LastResponseID, "resp_live")
	}

	select {
	case req := <-captured:
		if req.PreviousResponseID != "" {
			t.Fatalf("compaction previous_response_id = %q, want empty", req.PreviousResponseID)
		}
		if len(req.Input) < 4 {
			t.Fatalf("compaction input length = %d, want full history plus prompt", len(req.Input))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for compaction request capture")
	}
}

func TestHandoverUsesForkedProviderConversationState(t *testing.T) {
	provider, liveProvider, captured := newIsolatedResponsesProvider(t, "handover doc", "resp_handover")

	result, err := Handover(context.Background(), provider, "gpt-5.2", "current sys", "new sys", []core.Message{
		core.UserText("hello"),
		core.AssistantText("hi"),
		core.UserText("prepare handover"),
		core.AssistantText("ready to hand over"),
	}, "source", "target", engine.DefaultCompactionConfig(), engine.HandoverOptions{AllowProviderFork: true})
	if err != nil {
		t.Fatalf("Handover failed: %v", err)
	}
	if result.Document != "handover doc" {
		t.Fatalf("document = %q, want %q", result.Document, "handover doc")
	}

	if rp.OpenAIResponsesClientForTest(liveProvider).LastResponseID != "resp_live" {
		t.Fatalf("live LastResponseID = %q, want %q", rp.OpenAIResponsesClientForTest(liveProvider).LastResponseID, "resp_live")
	}

	select {
	case req := <-captured:
		if req.PreviousResponseID != "resp_live" {
			t.Fatalf("handover previous_response_id = %q, want resp_live", req.PreviousResponseID)
		}
		if len(req.Input) != 2 {
			t.Fatalf("handover input length = %d, want developer policy plus user trigger: %#v", len(req.Input), req.Input)
		}
		if req.Input[0].Role != "developer" || !strings.Contains(fmt.Sprint(req.Input[0].Content), "handing over") {
			t.Fatalf("handover fork policy = %#v, want developer instruction", req.Input[0])
		}
		if req.Input[1].Role != "user" {
			t.Fatalf("handover fork trigger = %#v, want user message", req.Input[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handover request capture")
	}
}

func TestHandoverForkDoesNotInheritOpenAIAdvancedOptions(t *testing.T) {
	provider, liveProvider, captured := newIsolatedResponsesProvider(t, "handover doc", "resp_handover")
	rp.SetOpenAIModelAndResponsesOptionsForTest(liveProvider, "gpt-5.6-sol", core.ResponsesOptions{
		ReasoningMode: "pro",
		MultiAgent: core.MultiAgentOptions{
			Enabled:                true,
			EnabledSet:             true,
			MaxConcurrentSubagents: 4,
		},
		PromptCache: core.PromptCacheOptions{Mode: "explicit", TTL: "30m"},
	})

	_, err := Handover(context.Background(), provider, "gpt-5.6-sol", "current sys", "new sys", []core.Message{
		core.UserText("hello"),
		core.AssistantText("ready to hand over"),
	}, "source", "target", engine.DefaultCompactionConfig(), engine.HandoverOptions{AllowProviderFork: true})
	if err != nil {
		t.Fatalf("Handover failed: %v", err)
	}

	select {
	case req := <-captured:
		if req.PreviousResponseID != "resp_live" {
			t.Fatalf("handover previous_response_id = %q, want resp_live", req.PreviousResponseID)
		}
		if req.MultiAgent != nil || req.PromptCacheOptions != nil || (req.Reasoning != nil && (req.Reasoning.Mode != "" || req.Reasoning.Context != "")) {
			t.Fatalf("forked helper inherited advanced Responses options: reasoning=%+v multi_agent=%+v prompt_cache=%+v", req.Reasoning, req.MultiAgent, req.PromptCacheOptions)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handover request capture")
	}
}

func TestHandoverFallsBackWhenTranscriptIsPastProviderBoundary(t *testing.T) {
	provider, liveProvider, captured := newIsolatedResponsesProvider(t, "handover doc", "resp_handover")

	_, err := Handover(context.Background(), provider, "gpt-5.2", "current sys", "new sys", []core.Message{
		core.UserText("hello"),
		core.AssistantText("hi"),
		core.UserText("this turn has no provider response"),
	}, "source", "target", engine.DefaultCompactionConfig(), engine.HandoverOptions{AllowProviderFork: true})
	if err != nil {
		t.Fatalf("Handover failed: %v", err)
	}
	if rp.OpenAIResponsesClientForTest(liveProvider).LastResponseID != "resp_live" {
		t.Fatalf("live LastResponseID = %q, want resp_live", rp.OpenAIResponsesClientForTest(liveProvider).LastResponseID)
	}

	select {
	case req := <-captured:
		if req.PreviousResponseID != "" {
			t.Fatalf("fallback previous_response_id = %q, want empty", req.PreviousResponseID)
		}
		if len(req.Input) < 5 {
			t.Fatalf("fallback input length = %d, want full history and handover framing: %#v", len(req.Input), req.Input)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fallback handover request capture")
	}
}

func TestOpenAIHTTPInterruptThenResumeWithSeparateSteeringRows(t *testing.T) {
	var calls atomic.Int32
	cancelled := make(chan struct{})
	secondInput := make(chan []responses.ResponsesInputItem, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []responses.ResponsesInputItem `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(cancelled)
			return
		}
		secondInput <- request.Input
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"replacement\"}}\n\n")
	}))
	defer server.Close()
	p := rp.NewOpenAIProviderForTest("", "gpt-4.1", &responses.ResponsesClient{BaseURL: server.URL, HTTPClient: server.Client()})
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.Stream(ctx, core.Request{Messages: []core.Message{core.UserText("source")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not settle")
	}
	a := core.UserText("first steer")
	a.ClientMessageID = "a"
	b := core.UserText("second steer")
	b.ClientMessageID = "b"
	next, err := p.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("source"), core.AssistantText("partial"), a, b}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = next.Recv()
		if err != nil {
			break
		}
	}
	_ = next.Close()
	input := <-secondInput
	if len(input) != 4 {
		t.Fatalf("replacement lost distinct input rows: %+v", input)
	}
	if a.ClientMessageID != "a" || b.ClientMessageID != "b" {
		t.Fatal("provider rewrote message identity")
	}
}

// TestResponsesProvidersGuardReplayedWebSearchOnTheWire checks the request each
// Responses provider actually sends, so provider wiring cannot drift from the
// shared guard.
func TestResponsesProvidersGuardReplayedWebSearchOnTheWire(t *testing.T) {
	history := []core.Message{
		core.UserText("q"),
		{Role: core.RoleAssistant, Parts: []core.Part{replayPart(testWebSearchCallRaw), {Type: core.PartText, Text: "answer"}}},
		core.UserText("again"),
	}
	fnTool := core.ToolSpec{Name: "example_tool", Description: "example", Schema: map[string]any{"type": "object"}}

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
		stream     func(t *testing.T, req core.Request) wire
	}{
		"chatgpt": {searchType: "web_search", stream: func(t *testing.T, req core.Request) wire {
			origClient := *rp.ChatGPTHTTPClientForTest
			defer func() { *rp.ChatGPTHTTPClientForTest = origClient }()
			var got wire
			*rp.ChatGPTHTTPClientForTest = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: make(http.Header)}, nil
			})}
			provider := rp.NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
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
		"openai": {searchType: "web_search_preview", stream: func(t *testing.T, req core.Request) wire {
			var got wire
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(sse))
			}))
			defer ts.Close()
			provider := rp.NewOpenAIProviderForTest("test-key", "gpt-4.1", &responses.ResponsesClient{
				BaseURL: ts.URL, GetAuthHeader: func() string { return "Bearer test-key" }, HTTPClient: ts.Client(),
			})
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
				req           core.Request
				wantToolsJSON string   // exact tools array, when set
				wantToolTypes []string // tool types in order, when set
				wantChoice    string
			}{
				{
					name:          "replay without tools",
					req:           core.Request{Messages: history},
					wantToolsJSON: `[{"external_web_access":false,"type":"web_search"}]`,
					wantChoice:    `"none"`,
				},
				{
					name:          "replay with caller tool",
					req:           core.Request{Messages: history, Tools: []core.ToolSpec{fnTool}},
					wantToolTypes: []string{"web_search", "function"},
					wantChoice:    `{"mode":"auto","tools":[{"name":"example_tool","type":"function"}],"type":"allowed_tools"}`,
				},
				{
					name:          "replay with required choice",
					req:           core.Request{Messages: history, Tools: []core.ToolSpec{fnTool}, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceRequired}},
					wantToolTypes: []string{"web_search", "function"},
					wantChoice:    `{"mode":"required","tools":[{"name":"example_tool","type":"function"}],"type":"allowed_tools"}`,
				},
				{
					name:          "replay with none choice",
					req:           core.Request{Messages: history, Tools: []core.ToolSpec{fnTool}, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceNone}},
					wantToolTypes: []string{"web_search", "function"},
					wantChoice:    `"none"`,
				},
				{
					name:          "search enabled is unchanged",
					req:           core.Request{Messages: history, Search: true},
					wantToolsJSON: `[{"type":"` + p.searchType + `"}]`,
				},
				{
					name: "no replay is unchanged",
					req:  core.Request{Messages: []core.Message{core.UserText("q")}},
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
