package responsesproviders_test

import (
	"context"
	. "github.com/samsaffron/term-llm/internal/llm"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"

	"encoding/json"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"

	"net/http"
	"net/http/httptest"

	"strings"
	"testing"
	"time"
)

func TestChatGPTContextPolicy(t *testing.T) {
	defer RegisterChatGPTContextConfig(nil)
	for _, tc := range []struct {
		name                                                string
		model                                               string
		backend, maximum, provider, perModel, reserve, want int
	}{
		{name: "astra shipped", model: "gpt-6-astra", backend: 272000, maximum: 872000, want: 372000},
		{name: "sol shipped", model: "gpt-5.6-sol", backend: 272000, maximum: 872000, want: 372000},
		{name: "account ceiling", model: "gpt-6-astra", backend: 272000, maximum: 300000, want: 300000},
		{name: "provider override", model: "gpt-6-astra", backend: 272000, maximum: 872000, provider: 600000, want: 600000},
		{name: "model beats provider", model: "gpt-6-astra", backend: 272000, maximum: 872000, provider: 600000, perModel: 500000, want: 500000},
		{name: "clamp before reserve", model: "gpt-6-astra", backend: 272000, maximum: 872000, perModel: 1000000, reserve: 20000, want: 852000},
		{name: "unknown default", model: "preview", backend: 200000, want: 200000},
		{name: "unknown cannot grow without ceiling", model: "preview", backend: 200000, perModel: 500000, want: 200000},
		{name: "offline bundled ceiling", model: "gpt-6-astra", perModel: 1000000, want: 872000},
		{name: "legacy 5.4 budget", model: "gpt-5.4", backend: 272000, maximum: 1000000, want: 922000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			RegisterChatGPTContextConfig(map[string]config.ProviderConfig{"work": {Type: config.ProviderTypeChatGPT, ContextWindow: tc.provider, ModelConfigs: []config.ProviderModelConfig{{ID: tc.model, ContextWindow: tc.perModel, MaxOutputTokens: tc.reserve}}}})
			facts := ModelInfo{ID: tc.model, BackendContext: tc.backend, MaxContext: tc.maximum}
			got := rp.ResolveChatGPTContextForTest("work", facts)
			if got.InputLimit != tc.want {
				t.Fatalf("got %d want %d", got.InputLimit, tc.want)
			}
			if facts.InputLimit != 0 {
				t.Fatal("mutated backend facts")
			}
		})
	}
	RegisterChatGPTContextConfig(nil)
	if got := rp.ResolveChatGPTContextForTest("work", ModelInfo{ID: "gpt-6-astra"}).InputLimit; got != 372000 {
		t.Fatalf("reload did not clear overrides: %d", got)
	}
}

func TestChatGPTContextCacheStoresFactsAcrossConfigReload(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	defer RegisterChatGPTContextConfig(nil)
	facts := (rp.ChatGPTModelInfoForTest{Slug: "gpt-6-astra", ContextWindow: 272000, MaxContextWindow: 872000}).ToModelInfoForTest()
	if err := rp.SaveChatGPTModelsCacheForTest(rp.ChatGPTModelsCacheForTest{ClientVersion: rp.ChatGPTModelsClientVersionForTest, Models: []ModelInfo{facts}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []int{600000, 700000} {
		RegisterChatGPTContextConfig(map[string]config.ProviderConfig{"chatgpt": {ContextWindow: target}})
		models, _, err := CachedChatGPTModels()
		if err != nil {
			t.Fatal(err)
		}
		if models[0].InputLimit != target || models[0].RecommendedContext != 372000 || models[0].MaxContext != 872000 {
			t.Fatalf("resolved model: %+v", models[0])
		}
	}
	cache, err := rp.LoadChatGPTModelsCacheForTest(rp.ChatGPTModelsClientVersionForTest)
	if err != nil {
		t.Fatal(err)
	}
	if cache.Models[0].InputLimit != 272000 || cache.Models[0].ConfiguredContext != 0 {
		t.Fatalf("cache contains policy: %+v", cache.Models[0])
	}
}

func TestChatGPTModelInfoPrefersExplicitMetadata(t *testing.T) {
	got := (rp.ChatGPTModelInfoForTest{
		Slug:                     "gpt-5.6-luna",
		ID:                       "ignored-id",
		Title:                    "Luna title",
		Name:                     "Luna name",
		MaxInputTokens:           400,
		InputTokenLimit:          300,
		ContextWindow:            200,
		MaxContextWindow:         1000,
		SupportedReasoningLevels: rp.ChatGPTReasoningLevelsTest{"low", "medium", "max"},
		DefaultReasoningLevel:    "low",
		DefaultReasoningEffort:   "medium",
	}).ToModelInfoForTest()
	if got.ID != "gpt-5.6-luna" || got.DisplayName != "Luna title" || got.InputLimit != 400 {
		t.Fatalf("toModelInfo() = %#v", got)
	}
	if got.OutputLimit != 128_000 {
		t.Fatalf("OutputLimit = %d, want static fallback 128000", got.OutputLimit)
	}
	if got.DefaultReasoningEffort != "medium" {
		t.Fatalf("DefaultReasoningEffort = %q, want explicit medium", got.DefaultReasoningEffort)
	}
}

func TestNewChatGPTProviderWithCredsDefaultsToGPT56SolMedium(t *testing.T) {
	t.Parallel()

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "")

	model, effort, _ := rp.ChatGPTModelEffortTierForTest(provider)
	if model != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want %q", model, "gpt-5.6-sol")
	}
	if effort != "medium" {
		t.Fatalf("effort = %q, want %q", effort, "medium")
	}
}

func TestGrokProviderExactEndpointHeadersAndResponsesFeatures(t *testing.T) {
	rp.IsolateGrokLLMTestEnvForTest(t)
	oldClient := (*rp.GrokHTTPClientForTest)
	defer func() { (*rp.GrokHTTPClientForTest) = oldClient }()
	var payload map[string]any
	(*rp.GrokHTTPClientForTest) = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != rp.GrokResponsesURLForTest {
			t.Fatalf("URL = %s, want %s", req.URL, rp.GrokResponsesURLForTest)
		}
		wantHeaders := map[string]string{
			"Accept":                   "text/event-stream",
			"Authorization":            "Bearer access",
			"X-XAI-Token-Auth":         "xai-grok-cli",
			"x-authenticateresponse":   "authenticate-response",
			"x-grok-client-mode":       "headless",
			"x-grok-client-version":    "1.0.6",
			"x-grok-client-identifier": "term-llm",
			"x-grok-model-override":    "grok-4.6",
			"x-grok-user-id":           "acct_1",
			"x-grok-conv-id":           "session_1",
			"x-grok-session-id":        "session_1",
			"User-Agent":               "term-llm",
		}
		for name, want := range wantHeaders {
			if got := req.Header.Get(name); got != want {
				t.Fatalf("header %s = %q, want %q", name, got, want)
			}
		}
		if requestID := req.Header.Get("x-grok-req-id"); !rp.ValidGrokUUIDv4ForTest(requestID) {
			t.Fatalf("x-grok-req-id = %q, want UUIDv4", requestID)
		}
		for _, name := range []string{"x-grok-agent-id", "x-grok-turn-idx"} {
			if got := req.Header.Get(name); got != "" {
				t.Fatalf("unexpected %s header %q", name, got)
			}
		}
		if req.Header.Get("session_id") != "" {
			t.Fatalf("unexpected generic session_id header %q", req.Header.Get("session_id"))
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return rp.GrokSSEForTest(
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[]}}`,
			`event: response.reasoning_summary_text.delta`,
			`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"thinking"}`,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[{"type":"summary_text","text":"thinking"}]}}`,
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"shell"}}`,
			`event: response.function_call_arguments.delta`,
			`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"command\":\"pwd\"}"}`,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":\"pwd\"}"}}`,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`,
			`data: [DONE]`,
		), nil
	})}
	provider := NewGrokProviderWithCreds(&credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}, "grok-4.6-high")
	rp.ResetGrokResponsesClientForTest(provider, rp.GrokCredsForTest(provider), "must-not-send")
	stream, err := provider.Stream(context.Background(), Request{
		SessionID: "session_1",
		Messages: []Message{{Role: RoleUser, Parts: []Part{
			{Type: PartText, Text: "look"},
			{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
		}}},
		Tools:           []ToolSpec{{Name: "shell", Description: "run", Schema: map[string]any{"type": "object"}}},
		MaxOutputTokens: 123,
		Temperature:     0.7,
		TemperatureSet:  true,
		TopP:            0.8,
		TopPSet:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	seen := map[EventType]bool{}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		seen[event.Type] = true
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		if event.Type == EventDone {
			break
		}
	}
	for _, typ := range []EventType{EventReasoningDelta, EventTextDelta, EventToolCall, EventUsage, EventProviderReplay, EventDone} {
		if !seen[typ] {
			t.Errorf("missing event type %s", typ)
		}
	}
	if payload["store"] != false || payload["max_output_tokens"].(float64) != 123 {
		t.Fatalf("payload controls = %v", payload)
	}
	if payload["parallel_tool_calls"] != true || payload["tool_choice"] != "auto" {
		t.Fatalf("tool protocol controls = %v", payload)
	}
	text, _ := payload["text"].(map[string]any)
	if text["verbosity"] != "low" {
		t.Fatalf("text controls = %v", payload)
	}
	if payload["instructions"] != rp.GrokDefaultInstructionsForTest {
		t.Fatalf("default instructions = %q, want %q", payload["instructions"], rp.GrokDefaultInstructionsForTest)
	}
	if payload["prompt_cache_key"] != "session_1" {
		t.Fatalf("prompt_cache_key = %v, want session_1", payload["prompt_cache_key"])
	}
	if got := payload["temperature"]; got != float64(float32(0.7)) {
		t.Fatalf("temperature = %v, want %v", got, float64(float32(0.7)))
	}
	if got := payload["top_p"]; got != float64(float32(0.8)) {
		t.Fatalf("top_p = %v, want %v", got, float64(float32(0.8)))
	}
	if _, ok := payload["previous_response_id"]; ok {
		t.Fatalf("previous_response_id sent: %v", payload)
	}
	encoded, _ := json.Marshal(payload)
	for _, want := range []string{"input_image", "data:image/png;base64,aGVsbG8=", "reasoning.encrypted_content", `"shell"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("request missing %q: %s", want, encoded)
		}
	}
}

func TestOpenAIProviderStreamReasoningEffortPrecedence(t *testing.T) {
	tests := []struct {
		name           string
		providerModel  string
		providerEffort string
		requestModel   string
		requestEffort  string
		wantEffort     string
	}{
		{
			name:          "provider suffix sets effort",
			providerModel: "gpt-5.4-xhigh",
			wantEffort:    "xhigh",
		},
		{
			name:           "request suffix overrides provider effort",
			providerModel:  "gpt-5.4",
			providerEffort: "low",
			requestModel:   "gpt-5.4-high",
			wantEffort:     "high",
		},
		{
			name:           "request reasoning_effort field wins over provider effort and suffix",
			providerModel:  "gpt-5.4",
			providerEffort: "low",
			requestModel:   "gpt-5.4-medium",
			requestEffort:  "high",
			wantEffort:     "high",
		},
		{
			name:          "minimal effort passes through",
			providerModel: "gpt-5.4",
			requestEffort: "minimal",
			wantEffort:    "minimal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				Reasoning *ResponsesReasoning `json:"reasoning,omitempty"`
			}

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer ts.Close()

			actualModel, effort := ParseModelEffort(tt.providerModel)
			provider := rp.NewOpenAIProviderRawForTest("test-key", actualModel, effort, &ResponsesClient{
				BaseURL:       ts.URL,
				GetAuthHeader: func() string { return "Bearer test-key" },
				HTTPClient:    ts.Client(),
			}, ResponsesOptions{})
			if tt.providerEffort != "" {
				rp.SetOpenAIEffortForTest(provider, tt.providerEffort)
			}

			stream, err := provider.Stream(context.Background(), Request{
				Model:           tt.requestModel,
				Messages:        []Message{UserText("hello")},
				ReasoningEffort: tt.requestEffort,
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}
			defer stream.Close()

			for {
				ev, err := stream.Recv()
				if err != nil {
					t.Fatalf("Recv() error = %v", err)
				}
				if ev.Type == EventDone {
					break
				}
			}

			if got.Reasoning == nil {
				t.Fatal("expected reasoning block to be sent")
			}
			if got.Reasoning.Effort != tt.wantEffort {
				t.Errorf("reasoning.effort = %q, want %q", got.Reasoning.Effort, tt.wantEffort)
			}
		})
	}
}

func TestOpenAIProviderStreamMapsGPT56AdvancedResponsesControls(t *testing.T) {
	var got ResponsesRequest
	var beta string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("OpenAI-Beta")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	}))
	defer ts.Close()

	provider := rp.NewOpenAIProviderRawForTest("test-key", "gpt-5.6-sol", "", &ResponsesClient{
		BaseURL:       ts.URL,
		GetAuthHeader: func() string { return "Bearer test-key" },
		HTTPClient:    ts.Client(),
	}, ResponsesOptions{
		ReasoningMode:    "standard",
		ReasoningContext: "all_turns",
		MultiAgent: MultiAgentOptions{
			Enabled:                true,
			EnabledSet:             true,
			MaxConcurrentSubagents: 6,
		},
		ProgrammaticToolCalling: ProgrammaticToolCallingOptions{
			Enabled:    true,
			EnabledSet: true,
			Tools:      []string{"shell"},
		},
		PromptCache: PromptCacheOptions{Mode: "implicit", TTL: "30m"},
	})

	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
		Tools: []ToolSpec{{
			Name:   "shell",
			Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		Responses: &ResponsesOptions{
			ReasoningMode:    "pro",
			ReasoningContext: "current_turn",
			MultiAgent:       MultiAgentOptions{MaxConcurrentSubagents: 2},
			PromptCache:      PromptCacheOptions{Mode: "explicit"},
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if got.Reasoning == nil || got.Reasoning.Mode != "pro" || got.Reasoning.Context != "current_turn" || got.Reasoning.Summary != "" {
		t.Fatalf("reasoning = %+v", got.Reasoning)
	}
	if got.MultiAgent == nil || !got.MultiAgent.Enabled || got.MultiAgent.MaxConcurrentSubagents != 2 {
		t.Fatalf("multi_agent = %+v", got.MultiAgent)
	}
	if got.PromptCacheOptions == nil || got.PromptCacheOptions.Mode != "explicit" || got.PromptCacheOptions.TTL != "30m" {
		t.Fatalf("prompt_cache_options = %+v", got.PromptCacheOptions)
	}
	if beta != "responses_multi_agent=v1" {
		t.Fatalf("OpenAI-Beta = %q", beta)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %#v", got.Tools)
	}
	tool, ok := got.Tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v", got.Tools[0])
	}
	callers, ok := tool["allowed_callers"].([]any)
	if !ok || len(callers) != 1 || callers[0] != "programmatic" {
		t.Fatalf("allowed_callers = %#v", tool["allowed_callers"])
	}
	marker, ok := got.Tools[1].(map[string]any)
	if !ok || marker["type"] != "programmatic_tool_calling" {
		t.Fatalf("PTC marker tool = %#v", got.Tools[1])
	}
}
