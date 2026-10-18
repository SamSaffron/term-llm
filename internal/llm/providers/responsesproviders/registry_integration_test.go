package responsesproviders_test

import (
	"context"
	. "github.com/samsaffron/term-llm/internal/llm"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"

	"encoding/json"
	"errors"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"

	"github.com/samsaffron/term-llm/internal/openaiapi"
	"io"
	"net/http"
	"net/http/httptest"

	"reflect"
	"slices"
	"strings"

	"testing"
	"time"
)

func TestChatGPTContextEngineUsesScopedAccountCeiling(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	defer RegisterChatGPTContextConfig(nil)
	RegisterChatGPTContextConfig(map[string]config.ProviderConfig{"work": {Type: config.ProviderTypeChatGPT, ContextWindow: 600000, ModelConfigs: []config.ProviderModelConfig{{ID: "gpt-6-astra", Alias: "astra"}}}})
	if err := rp.SaveChatGPTModelsCacheForTest(rp.ChatGPTModelsCacheForTest{AccountID: "test", FetchedAt: time.Now(), ClientVersion: rp.ChatGPTModelsClientVersionForTest, Models: []ModelInfo{{ID: "gpt-6-astra", BackendContext: 272000, MaxContext: 400000}}}); err != nil {
		t.Fatal(err)
	}
	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{AccountID: "test", AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}, "gpt-6-astra")
	eng := NewEngine(provider, NewToolRegistry())
	eng.ConfigureContextManagement(provider, "work", "astra-high", true)
	if got := eng.InputLimit(); got != 400000 {
		t.Fatalf("engine context %d want account maximum 400000", got)
	}
}

func TestChatGPTListModelsFallsBackToStaticCatalogOnColdFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	origClient := (*rp.ChatGPTHTTPClientForTest)
	defer func() { (*rp.ChatGPTHTTPClientForTest) = origClient }()
	(*rp.ChatGPTHTTPClientForTest) = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, "gpt-5.6-sol")

	models, fresh, err := provider.ListModelsWithFreshness(context.Background())
	if err != nil || fresh {
		t.Fatalf("ListModelsWithFreshness() = fresh %v, error %v", fresh, err)
	}
	if len(models) != len(ProviderModels["chatgpt"]) || models[0].ID != "gpt-6-astra" {
		t.Fatalf("static fallback = %#v", models)
	}
}

func TestChatGPTCachedCatalogDrivesModelsAndContextLimit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := rp.SaveChatGPTModelsCacheForTest(rp.ChatGPTModelsCacheForTest{
		FetchedAt:     time.Now(),
		ClientVersion: rp.ChatGPTModelsClientVersionForTest,
		Models:        []ModelInfo{{ID: "account-preview", InputLimit: 333_000}},
	}); err != nil {
		t.Fatalf("save cache: %v", err)
	}
	ids := ProviderModelIDs("chatgpt")
	if len(ids) != 1 || ids[0] != "account-preview" {
		t.Fatalf("ProviderModelIDs(chatgpt) = %v", ids)
	}
	if got := InputLimitForProviderModel("chatgpt", "account-preview-high"); got != 333_000 {
		t.Fatalf("dynamic input limit = %d, want 333000", got)
	}
}

func TestChatGPTModelSuffixes(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if model, tier := rp.ChatGPTModelServiceTierForTest("gpt-reserve-low-fast"); model != "gpt-reserve-low-fast" || tier != "" {
		t.Fatalf("without metadata, got (%q, %q)", model, tier)
	}
	models := []ModelInfo{
		{ID: "gpt-reserve", InputLimit: 272000, ReasoningEfforts: []string{"low", "medium", "max"}, ServiceTiers: []ModelServiceTier{{ID: "priority"}}},
		{ID: "slow", ReasoningEfforts: []string{"low"}},
		{ID: "speed", ReasoningEfforts: []string{"low"}, AdditionalSpeedTiers: []string{"fast"}},
		{ID: "literal", ServiceTiers: []ModelServiceTier{{ID: "priority"}}},
		{ID: "literal-fast"},
	}
	if err := rp.SaveChatGPTModelsCacheForTest(rp.ChatGPTModelsCacheForTest{ClientVersion: rp.ChatGPTModelsClientVersionForTest, Models: models}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, model, effort, tier string }{
		{"gpt-reserve-low", "gpt-reserve", "low", ""},
		{"gpt-reserve-low-fast", "gpt-reserve", "low", ServiceTierFast},
		{"gpt-reserve-fast", "gpt-reserve", "", ServiceTierFast},
		{"gpt-reserve-max-fast", "gpt-reserve", "max", ServiceTierFast},
		{"gpt-reserve-high-fast", "gpt-reserve-high-fast", "", ""},
		{"slow-low-fast", "slow-low-fast", "", ""},
		{"speed-low-fast", "speed", "low", ServiceTierFast},
		{"literal-fast", "literal-fast", "", ""},
		{"unknown-low-fast", "unknown-low-fast", "", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			p := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{}, tc.input)
			model, effort, tier := rp.ChatGPTModelEffortTierForTest(p)
			if model != tc.model || effort != tc.effort || tier != tc.tier {
				t.Fatalf("got (%s, %s, %s), want (%s, %s, %s)", model, effort, tier, tc.model, tc.effort, tc.tier)
			}
		})
	}
	if got := InputLimitForProviderModel("chatgpt", "gpt-reserve-low-fast"); got != 272000 {
		t.Fatalf("input limit = %d", got)
	}
	if got := ReasoningEffortsForProviderModel("chatgpt", "gpt-reserve-low"); !equalSlice(got, models[0].ReasoningEfforts) {
		t.Fatalf("efforts = %v", got)
	}

	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{"", []string{"gpt-reserve", "slow", "speed", "literal", "literal-fast"}},
		{"gpt-res", []string{"gpt-reserve"}},
		{"gpt-reserve", []string{"gpt-reserve", "gpt-reserve-fast", "gpt-reserve-low", "gpt-reserve-medium", "gpt-reserve-max"}},
		{"gpt-reserve-l", []string{"gpt-reserve-low"}},
		{"gpt-reserve-low", []string{"gpt-reserve-low", "gpt-reserve-low-fast"}},
		{"gpt-reserve-low-f", []string{"gpt-reserve-low-fast"}},
		{"gpt-reserve-low-fast", []string{"gpt-reserve-low-fast"}},
	} {
		got := GetProviderCompletions("chatgpt:"+tc.prefix, false, nil)
		want := make([]string, len(tc.want))
		for i, model := range tc.want {
			want[i] = "chatgpt:" + model
		}
		if !equalSlice(got, want) {
			t.Fatalf("completion for %q = %v, want %v", tc.prefix, got, want)
		}
	}

	for _, prefix := range []string{"gpt-reserve-low", "gpt-reserve-low-f", "speed-low"} {
		got := GetProviderCompletions("chatgpt:"+prefix, false, nil)
		want := "chatgpt:" + strings.TrimSuffix(prefix, "-f") + "-fast"
		if !slices.Contains(got, want) {
			t.Fatalf("completion for %q = %v, missing %q", prefix, got, want)
		}
	}
	if got := GetProviderCompletions("chatgpt:slow-low", false, nil); slices.Contains(got, "chatgpt:slow-low-fast") {
		t.Fatalf("unsupported fast completion: %v", got)
	}
	if got := GetProviderCompletions("chatgpt:literal-fast", false, nil); !equalSlice(got, []string{"chatgpt:literal-fast"}) {
		t.Fatalf("literal ID completion duplicated or expanded: %v", got)
	}

	origClient := (*rp.ChatGPTHTTPClientForTest)
	t.Cleanup(func() { (*rp.ChatGPTHTTPClientForTest) = origClient })
	var captured ResponsesRequest
	(*rp.ChatGPTHTTPClientForTest) = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = ResponsesRequest{}
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\ndata: [DONE]\n\n"))}, nil
	})}
	for _, tc := range []struct {
		name, model  string
		req          Request
		tier, effort string
	}{
		{"constructor", "gpt-reserve-low-fast", Request{}, ServiceTierFast, "low"},
		{"request", "gpt-reserve", Request{Model: "gpt-reserve-low-fast"}, ServiceTierFast, "low"},
		{"explicit overrides", "gpt-reserve-low-fast", Request{ReasoningEffort: "max", ServiceTierSet: true}, "", "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}, tc.model)
			tc.req.Messages = []Message{UserText("hi")}
			stream, err := p.Stream(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			drainStreamToDone(t, stream)
			if captured.Model != "gpt-reserve" || captured.Reasoning.Effort != tc.effort || captured.ServiceTier != tc.tier {
				t.Fatalf("unexpected request: %+v", captured)
			}
		})
	}
}

func TestCopilotListModelsFetchesLiveModelsAndCachesThem(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	origClient := (*rp.CopilotHTTPClientForTest)
	t.Cleanup(func() { (*rp.CopilotHTTPClientForTest) = origClient })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer session-token" {
			t.Fatalf("Authorization header = %q, want Bearer session-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"data": [
				{"id":"dynamic-copilot-model","name":"Dynamic Copilot Model","vendor":"github","capabilities":{"limits":{"max_prompt_tokens":123456,"max_context_window_tokens":200000,"max_output_tokens":64000}}},
				{"id":"dynamic-preview-model","name":"Dynamic Preview","vendor":"openai","preview":true},
				{"id":"gpt-5.5","name":"GPT 5.5","vendor":"openai","capabilities":{"limits":{"max_context_window_tokens":1050000,"max_output_tokens":128000}}}
			]
		}`)
	}))
	defer server.Close()
	(*rp.CopilotHTTPClientForTest) = server.Client()

	provider := rp.CopilotProviderForTest(&credentials.CopilotCredentials{AccessToken: "oauth-token"}, server.URL, "session-token", time.Now().Add(time.Hour))

	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("models len = %d, want 3", len(models))
	}
	if models[0].ID != "dynamic-copilot-model" || models[0].DisplayName != "Dynamic Copilot Model" || models[0].OwnedBy != "github" {
		t.Fatalf("first model not parsed as expected: %+v", models[0])
	}
	if models[0].InputLimit != 123_456 {
		t.Fatalf("dynamic prompt token limit = %d, want 123456", models[0].InputLimit)
	}
	if models[1].DisplayName != "Dynamic Preview (preview)" {
		t.Fatalf("preview display name = %q, want Dynamic Preview (preview)", models[1].DisplayName)
	}
	if models[2].ID != "gpt-5.5" || models[2].InputLimit != 1_030_000 {
		t.Fatalf("gpt-5.5 list metadata = %+v, want 1,030,000 input limit", models[2])
	}

	ids := ProviderModelIDs("copilot")
	if !containsModelID(ids, "dynamic-copilot-model") || !containsModelID(ids, "dynamic-preview-model") || !containsModelID(ids, "gpt-5.5") {
		t.Fatalf("cached ProviderModelIDs(copilot) = %v, want fetched dynamic models", ids)
	}
	if got := InputLimitForProviderModel("copilot", "gpt-5.5"); got != 1_030_000 {
		t.Fatalf("cached copilot gpt-5.5 input limit = %d, want 1030000", got)
	}
}

func TestGrokCatalogAuthenticatedFilteringBoundsAndCache(t *testing.T) {
	rp.IsolateGrokLLMTestEnvForTest(t)
	oldClient := (*rp.GrokHTTPClientForTest)
	defer func() { (*rp.GrokHTTPClientForTest) = oldClient }()
	calls := 0
	(*rp.GrokHTTPClientForTest) = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != rp.GrokModelsURLForTest {
			t.Fatalf("catalog URL = %s", req.URL)
		}
		for name, want := range map[string]string{
			"Authorization":         "Bearer access",
			"X-XAI-Token-Auth":      "xai-grok-cli",
			"x-userid":              "acct_1",
			"x-grok-client-version": "1.0.6",
			"x-grok-client-mode":    "headless",
			"User-Agent":            "term-llm",
			"Accept":                "application/json",
		} {
			if got := req.Header.Get(name); got != want {
				t.Fatalf("header %s = %q, want %q", name, got, want)
			}
		}
		body := `{"data":[` +
			`{"id":"grok-4.6","model":"grok-4.6","api_backend":"responses","name":"Grok 4.6","context_window":500000,"max_completion_tokens":8000,"supports_reasoning_effort":true,"reasoning_efforts":[{"value":"high"},{"value":"xhigh"}]},` +
			`{"id":"legacy","model":"legacy","api_backend":"chat_completions","context_window":1000},` +
			`{"id":"missing-backend","model":"missing-backend","context_window":1000},` +
			`{"id":"future","model":"future","api_backend":"ReSpOnSeS","context_window":2000,"supports_reasoning_effort":true,"reasoning_efforts":[{"value":"low"}]}` + `]}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	if err := credentials.SaveGrokCredentials(creds); err != nil {
		t.Fatal(err)
	}
	provider := NewGrokProviderWithCreds(creds, "grok-4.6")
	if got := InputLimitForProviderModel("grok", "grok-4.6-high"); got != 192_000 {
		t.Fatalf("static Grok fallback input limit = %d, want 192000", got)
	}
	models, fresh, err := provider.ListModelsWithFreshness(context.Background())
	if err != nil || !fresh {
		t.Fatalf("ListModelsWithFreshness = fresh %v err %v", fresh, err)
	}
	if len(models) != 2 || models[0].ID != "grok-4.6" || models[1].ID != "future" {
		t.Fatalf("filtered models = %+v", models)
	}
	if models[0].InputLimit != 500_000 || models[0].OutputLimit != 8_000 {
		t.Fatalf("catalog limits = input %d output %d", models[0].InputLimit, models[0].OutputLimit)
	}
	if got := strings.Join(models[0].ReasoningEfforts, ","); got != "high,xhigh" {
		t.Fatalf("known model efforts = %q", got)
	}
	if got := strings.Join(models[1].ReasoningEfforts, ","); got != "low" {
		t.Fatalf("future model live efforts = %q", got)
	}
	if got := InputLimitForProviderModel("grok", "grok-4.6-high"); got != 500_000 {
		t.Fatalf("live cached input limit = %d, want 500000 instead of static 192000", got)
	}
	if got := strings.Join(ReasoningEffortsForProviderModel("grok", "grok-4.6"), ","); got != "high,xhigh" {
		t.Fatalf("live cached reasoning efforts = %q", got)
	}
	if _, _, err := provider.ListModelsWithFreshness(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("catalog HTTP calls = %d, want cache hit", calls)
	}
	ids := ProviderModelIDs("grok")
	if len(ids) != 2 || ids[0] != "grok-4.6" || ids[1] != "future" {
		t.Fatalf("picker/cache IDs = %v", ids)
	}
}

func TestGrokStaticReasoningEffortsMatchCanonicalMetadata(t *testing.T) {
	rp.IsolateGrokLLMTestEnvForTest(t)
	if got := strings.Join(ReasoningEffortsForProviderModel("grok", "grok-4.6"), ","); got != "low,medium,high,xhigh" {
		t.Fatalf("grok-4.6 fallback efforts = %q", got)
	}
	if got := strings.Join(ReasoningEffortsForProviderModel("grok", "grok-4.5"), ","); got != "low,medium,high" {
		t.Fatalf("grok-4.5 fallback efforts = %q, want no xhigh", got)
	}
}

func TestConfiguredGrokAliasUsesNativeProvider(t *testing.T) {
	rp.IsolateGrokLLMTestEnvForTest(t)
	if err := credentials.SaveGrokCredentials(&credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}); err != nil {
		t.Fatal(err)
	}
	provider, err := createProviderFromConfig("personal-grok", &config.ProviderConfig{Type: config.ProviderTypeGrok, Model: "grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*GrokProvider); !ok {
		t.Fatalf("configured type grok created %T", provider)
	}
}

func TestGrokConfiguredAPIFieldsRequireExplicitProviderType(t *testing.T) {
	rp.IsolateGrokLLMTestEnvForTest(t)
	for _, cfg := range []config.ProviderConfig{
		{APIKey: "legacy-key", Model: "grok-4"},
		{BaseURL: "https://api.x.ai/v1", Model: "grok-4"},
	} {
		_, err := createProviderFromConfig("grok", &cfg)
		if err == nil || !strings.Contains(err.Error(), "type: xai") || !strings.Contains(err.Error(), "type: openai_compatible") {
			t.Fatalf("compatibility error = %v", err)
		}
	}
}

func TestGrokProviderReplaysEncryptedReasoningThroughToolLoopWithoutServerState(t *testing.T) {
	rp.IsolateGrokLLMTestEnvForTest(t)
	oldClient := (*rp.GrokHTTPClientForTest)
	defer func() { (*rp.GrokHTTPClientForTest) = oldClient }()

	var payloads []map[string]any
	(*rp.GrokHTTPClientForTest) = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
		switch len(payloads) {
		case 1:
			return rp.GrokSSEForTest(
				`event: response.output_item.added`,
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[]}}`,
				`event: response.output_item.done`,
				// Completed provider items already carry reasoning_text. Opaque replay
				// must preserve that valid discriminator byte-for-byte.
				`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[],"content":[{"type":"reasoning_text","text":"private reasoning"}]}}`,
				`event: response.output_item.added`,
				`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"count_tool"}}`,
				`event: response.function_call_arguments.delta`,
				`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
				`event: response.output_item.done`,
				`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"count_tool","arguments":"{}"}}`,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`,
				`data: [DONE]`,
			), nil
		case 2:
			return rp.GrokSSEForTest(
				`event: response.output_text.delta`,
				`data: {"type":"response.output_text.delta","delta":"done"}`,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":8,"output_tokens":1,"total_tokens":9}}}`,
				`data: [DONE]`,
			), nil
		default:
			t.Fatalf("unexpected Grok request %d", len(payloads))
			return nil, nil
		}
	})}

	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	provider := NewGrokProviderWithCreds(creds, "grok-4.6")
	rp.ResetGrokResponsesClientForTest(provider, creds, "must-not-send")
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(provider, registry)

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("use the tool")},
		Tools:    []ToolSpec{tool.Spec()},
		MaxTurns: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := drainStreamErr(t, stream); err != nil {
		t.Fatalf("tool loop stream: %v", err)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("tool executions = %d, want 1", tool.calls.Load())
	}
	if len(payloads) != 2 {
		t.Fatalf("Grok requests = %d, want 2", len(payloads))
	}
	for i, payload := range payloads {
		if payload["store"] != false {
			t.Fatalf("request %d store = %v, want false", i+1, payload["store"])
		}
		if _, ok := payload["previous_response_id"]; ok {
			t.Fatalf("request %d sent previous_response_id: %v", i+1, payload)
		}
	}

	input, ok := payloads[1]["input"].([]any)
	if !ok {
		t.Fatalf("second request input = %#v", payloads[1]["input"])
	}
	var reasoning, functionCall, functionOutput map[string]any
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "reasoning":
			reasoning = item
		case "function_call":
			functionCall = item
		case "function_call_output":
			functionOutput = item
		}
	}
	if reasoning == nil || reasoning["id"] != "rs_1" || reasoning["encrypted_content"] != "enc_1" {
		t.Fatalf("reasoning replay item = %#v", reasoning)
	}
	content, ok := reasoning["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("reasoning replay content = %#v", reasoning["content"])
	}
	reasoningText, ok := content[0].(map[string]any)
	if !ok || reasoningText["type"] != "reasoning_text" || reasoningText["text"] != "private reasoning" {
		t.Fatalf("reasoning replay content item = %#v", content[0])
	}
	if functionCall == nil || functionCall["call_id"] != "call_1" || functionCall["name"] != "count_tool" || functionCall["arguments"] != "{}" {
		t.Fatalf("function call replay item = %#v", functionCall)
	}
	if functionOutput == nil || functionOutput["call_id"] != "call_1" || functionOutput["output"] != "ok" {
		t.Fatalf("function_call_output = %#v", functionOutput)
	}
}

func TestGrokEncryptedContent400IsNotRetriedByOuterProvider(t *testing.T) {
	rp.IsolateGrokLLMTestEnvForTest(t)
	oldClient := (*rp.GrokHTTPClientForTest)
	t.Cleanup(func() { (*rp.GrokHTTPClientForTest) = oldClient })

	calls := 0
	(*rp.GrokHTTPClientForTest) = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"message":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}}`,
			)),
		}, nil
	})}
	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	provider := WrapWithRetry(NewGrokProviderWithCreds(creds, "grok-4.6"), RetryConfig{
		MaxAttempts: 3,
		BaseBackoff: time.Nanosecond,
		MaxBackoff:  time.Nanosecond,
	})
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("continue")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	streamErr := drainStreamErr(t, stream)
	if streamErr == nil || !strings.Contains(streamErr.Error(), "encrypted_content") {
		t.Fatalf("stream error = %v", streamErr)
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want 1 for non-transient HTTP 400", calls)
	}
}

// A custom-named type: openai provider must translate effort suffixes that
// the user declared via models[].reasoning_efforts in config into
// reasoning.effort, instead of sending the suffixed name as the model ID.
// Undeclared suffixes stay part of the model ID.
func TestOpenAIProviderConfigDeclaredEffortSuffixForCustomProvider(t *testing.T) {
	rp.ClearOpenAIEndpointEnvForTest(t)
	// Mirrors registerModelLimits on config load.
	RegisterConfigReasoningEfforts([]ConfigModelReasoningEfforts{{
		Provider: "responses-custom",
		Model:    "vendor-model",
		Efforts:  []string{"low", "high", "xhigh"},
	}})
	defer RegisterConfigReasoningEfforts(nil)

	for _, tc := range []struct {
		name          string
		providerModel string
		requestModel  string
		requestEffort string
		wantModel     string
		wantEffort    string
	}{
		{name: "provider model suffix", providerModel: "vendor-model-xhigh", wantModel: "vendor-model", wantEffort: "xhigh"},
		{name: "request model suffix", providerModel: "vendor-model", requestModel: "vendor-model-high", wantModel: "vendor-model", wantEffort: "high"},
		{name: "bare declared model sends no effort", providerModel: "vendor-model", wantModel: "vendor-model", wantEffort: ""},
		{name: "undeclared provider suffix preserved", providerModel: "vendor-model-max", wantModel: "vendor-model-max", wantEffort: ""},
		{name: "undeclared request suffix preserved", providerModel: "vendor-model", requestModel: "vendor-model-max", wantModel: "vendor-model-max", wantEffort: ""},
		{name: "request suffix overrides provider suffix", providerModel: "vendor-model-low", requestModel: "vendor-model-high", wantModel: "vendor-model", wantEffort: "high"},
		{name: "explicit request effort overrides both suffixes", providerModel: "vendor-model-low", requestModel: "vendor-model-high", requestEffort: "xhigh", wantModel: "vendor-model", wantEffort: "xhigh"},
		// Built-in GPT effort tables still apply under a custom provider name.
		{name: "gpt suffix under custom name", providerModel: "gpt-5.4-xhigh", wantModel: "gpt-5.4", wantEffort: "xhigh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ResponsesRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n"))
			}))
			defer server.Close()

			created, err := createProviderFromConfig("responses-custom", &config.ProviderConfig{
				Type: config.ProviderTypeOpenAI, APIKey: "test-key", Model: tc.providerModel, BaseURL: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := created.Stream(context.Background(), Request{Model: tc.requestModel, Messages: []Message{UserText("hello")}, ReasoningEffort: tc.requestEffort})
			if err != nil {
				t.Fatal(err)
			}
			drainStreamToDone(t, stream)
			stream.Close()

			if got.Model != tc.wantModel {
				t.Errorf("request model = %q, want %q", got.Model, tc.wantModel)
			}
			gotEffort := ""
			if got.Reasoning != nil {
				gotEffort = got.Reasoning.Effort
			}
			if gotEffort != tc.wantEffort {
				t.Errorf("request reasoning effort = %q, want %q", gotEffort, tc.wantEffort)
			}
		})
	}
}

func TestCreateOpenAIProviderFromConfigUsesBaseURLAndHeaders(t *testing.T) {
	rp.ClearOpenAIEndpointEnvForTest(t)
	t.Setenv(openaiapi.EnvProject, "proj-env")
	provider, err := createProviderFromConfig("openai", &config.ProviderConfig{
		ResolvedAPIKey: "test-key",
		Model:          "gpt-5.2",
		BaseURL:        "https://gateway.example/v1/",
		Headers:        map[string]string{"OpenAI-Organization": "org-config"},
	})
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	openai, ok := provider.(*OpenAIProvider)
	if !ok {
		t.Fatalf("provider = %T", provider)
	}
	want := openaiapi.Endpoint{
		BaseURL: "https://gateway.example/v1",
		Headers: map[string]string{"OpenAI-Organization": "org-config", "OpenAI-Project": "proj-env"},
	}
	if !reflect.DeepEqual(rp.OpenAIEndpointForTest(openai), want) {
		t.Fatalf("endpoint = %#v, want %#v", rp.OpenAIEndpointForTest(openai), want)
	}
}

func TestOpenAIProviderRequestEncryptedReasoning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		option      *bool
		wantInclude bool
	}{
		{name: "default", wantInclude: true},
		{name: "enabled", option: boolPtr(true), wantInclude: true},
		{name: "disabled", option: boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []ResponsesRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload ResponsesRequest
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests = append(requests, payload)
				if len(requests) == 2 {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":"previous_response_id not found"}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_saved\",\"output\":[]}}\n\n"))
			}))
			defer server.Close()

			// Exercise the custom type: openai configuration path, not just the constructor.
			created, err := createProviderFromConfig("muse_wonder", &config.ProviderConfig{
				Type: config.ProviderTypeOpenAI, APIKey: "test-key", Model: "muse-wonder",
				BaseURL: server.URL, RequestEncryptedReasoning: tc.option,
			})
			if err != nil {
				t.Fatal(err)
			}
			provider := created.(*OpenAIProvider)
			messages := []Message{UserText("hello")}
			stream, err := provider.Stream(context.Background(), Request{Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			drainStreamToDone(t, stream)
			stream.Close()

			// Existing or unsolicited encrypted reasoning must still survive full-history replay.
			messages = append(messages, Message{Role: RoleAssistant, Parts: []Part{{
				Type: PartText, Text: "answer", ReasoningItemID: "rs_saved", ReasoningEncryptedContent: "enc_saved",
			}}}, UserText("follow up"))
			stream, err = provider.Stream(context.Background(), Request{Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			drainStreamToDone(t, stream)
			stream.Close()

			if len(requests) != 3 {
				t.Fatalf("requests = %d, want initial, continuation, and full-history retry", len(requests))
			}
			for i, req := range requests {
				want := []string(nil)
				if tc.wantInclude {
					want = []string{"reasoning.encrypted_content"}
				}
				if !reflect.DeepEqual(req.Include, want) {
					t.Errorf("request %d include = %v, want %v", i, req.Include, want)
				}
			}
			if requests[0].PreviousResponseID != "" || requests[1].PreviousResponseID != "resp_saved" || requests[2].PreviousResponseID != "" {
				t.Fatalf("unexpected continuation state: %q, %q, %q", requests[0].PreviousResponseID, requests[1].PreviousResponseID, requests[2].PreviousResponseID)
			}
			if len(requests[1].Input) != 1 {
				t.Fatalf("continuation input = %v, want only latest user message", requests[1].Input)
			}
			var found bool
			for _, item := range requests[2].Input {
				if item.Type == "reasoning" && item.EncryptedContent == "enc_saved" {
					found = true
				}
			}
			if !found {
				t.Fatal("full-history retry lost saved encrypted reasoning")
			}
		})
	}
}

func TestOpenAIProviderUsePreviousResponseID(t *testing.T) {
	for _, useWebSocket := range []bool{false, true} {
		for _, tc := range []struct {
			name             string
			option           *bool
			wantContinuation bool
		}{
			{name: "default", wantContinuation: true},
			{name: "enabled", option: boolPtr(true), wantContinuation: true},
			{name: "disabled", option: boolPtr(false)},
		} {
			transport := "http"
			if useWebSocket {
				transport = "websocket"
			}
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				requests := make(chan ResponsesRequest, 2)
				upgrader := websocket.Upgrader{}
				completed := map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_saved", "output": []any{}}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if useWebSocket {
						conn, err := upgrader.Upgrade(w, r, nil)
						if err != nil {
							t.Errorf("upgrade: %v", err)
							return
						}
						defer conn.Close()
						for range 2 {
							var payload ResponsesRequest
							if err := conn.ReadJSON(&payload); err != nil {
								t.Errorf("read WebSocket request: %v", err)
								return
							}
							requests <- payload
							if err := conn.WriteJSON(completed); err != nil {
								t.Errorf("write WebSocket response: %v", err)
								return
							}
						}
						return
					}
					var payload ResponsesRequest
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode request: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					requests <- payload
					w.Header().Set("Content-Type", "text/event-stream")
					body, _ := json.Marshal(completed)
					_, _ = w.Write(append(append([]byte("data: "), body...), '\n', '\n'))
				}))
				defer server.Close()
				created, err := createProviderFromConfig("muse_wonder", &config.ProviderConfig{
					Type: config.ProviderTypeOpenAI, APIKey: "test-key", Model: "muse-wonder",
					BaseURL: server.URL, UseWebSocket: useWebSocket, UsePreviousResponseID: tc.option,
				})
				if err != nil {
					t.Fatal(err)
				}
				provider := created.(*OpenAIProvider)
				defer func() { rp.OpenAIResponsesClientForTest(provider).CloseWebSocket() }()
				messages := []Message{UserText("old question"), {Role: RoleAssistant, Parts: []Part{{
					Type: PartText, Text: "old answer", ReasoningItemID: "rs_saved", ReasoningEncryptedContent: "enc_saved",
				}}}, UserText("hello")}
				for range 2 {
					stream, err := provider.Stream(context.Background(), Request{Messages: messages})
					if err != nil {
						t.Fatal(err)
					}
					drainStreamToDone(t, stream)
					stream.Close()
					messages = append(messages, UserText("follow up"))
				}
				first, second := <-requests, <-requests
				if first.PreviousResponseID != "" {
					t.Fatalf("initial request previous_response_id = %q", first.PreviousResponseID)
				}
				if tc.wantContinuation {
					if second.PreviousResponseID != "resp_saved" || len(second.Input) != 1 {
						t.Fatalf("expected response-ID continuation, got id=%q input=%v", second.PreviousResponseID, second.Input)
					}
				} else {
					if second.PreviousResponseID != "" || len(second.Input) != len(first.Input)+1 {
						t.Fatalf("expected full-history replay, got id=%q input=%v", second.PreviousResponseID, second.Input)
					}
					if second.Input[1].Type != "reasoning" || second.Input[1].EncryptedContent != "enc_saved" {
						t.Fatal("full-history replay lost encrypted reasoning")
					}
				}
				if !reflect.DeepEqual(second.Include, []string{"reasoning.encrypted_content"}) {
					t.Fatalf("encrypted reasoning default changed: include=%v", second.Include)
				}
			})
		}
	}
}

func TestOpenCodeGoListModelsMergesLiveCatalog(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, recorder := rp.NewOpenCodeGoTestServerForTest(t)
	defer server.Close()

	provider := rp.NewOpenCodeGoProviderForTest("test-key", "chat-model", server.URL, server.URL+"/catalog", server.Client())
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 7 { // deprecated-model is intentionally hidden
		t.Fatalf("models = %#v, want seven active models", models)
	}
	byID := make(map[string]ModelInfo)
	for _, model := range models {
		byID[model.ID] = model
	}
	if got := byID["responses-model"]; got.DisplayName != "Responses Model" || got.InputLimit != 900 || got.InputPrice != 1.25 {
		t.Fatalf("enriched responses model = %#v", got)
	}
	if got := byID["messages-model"]; got.InputPrice != -1 || got.OutputPrice != -1 {
		t.Fatalf("missing cost decoded as %g/%g, want unknown -1/-1", got.InputPrice, got.OutputPrice)
	}
	if got := byID["qwen3.8-max"].ReasoningEfforts; strings.Join(got, ",") != "high,max" {
		t.Fatalf("qwen3.8-max reasoning efforts = %v, want high/max", got)
	}
	if got := byID["unknown-preview"].DisplayName; got != "" {
		t.Fatalf("unknown preview display name = %q, want empty fallback metadata", got)
	}
	if ids := GetCachedOpenCodeGoModels(); strings.Join(ids, ",") != "chat-model,gemini-model,messages-model,muse-spark-1.2,qwen3.8-max,responses-model,unknown-preview" {
		t.Fatalf("cached IDs = %v", ids)
	}
	cachedInfos, fresh, err := CachedOpenCodeGoModels()
	if err != nil || !fresh {
		t.Fatalf("CachedOpenCodeGoModels = fresh %v, error %v", fresh, err)
	}
	cachedByID := make(map[string]ModelInfo, len(cachedInfos))
	for _, model := range cachedInfos {
		cachedByID[model.ID] = model
	}
	if got := cachedByID["qwen3.8-max"].ReasoningEfforts; strings.Join(got, ",") != "high,max" {
		t.Fatalf("cached qwen reasoning efforts = %v, want high/max", got)
	}
	if got := InputLimitForProviderModel("opencode-go", "responses-model"); got != 900 {
		t.Fatalf("cached input limit = %d, want 900", got)
	}
	if got := ReasoningEffortsForProviderModel("opencode-go", "responses-model"); strings.Join(got, ",") != "low,high" {
		t.Fatalf("cached reasoning efforts = %v, want low/high", got)
	}
	for _, tc := range []struct {
		model      string
		wantBase   string
		wantEffort string
	}{
		{model: "qwen3.8-max", wantBase: "qwen3.8-max"},
		{model: "qwen3.8-max-high", wantBase: "qwen3.8-max", wantEffort: "high"},
		{model: "qwen3.8-max-max", wantBase: "qwen3.8-max", wantEffort: "max"},
	} {
		base, effort := BaseModelAndEffortForProvider("opencode-go", tc.model)
		if base != tc.wantBase || effort != tc.wantEffort {
			t.Fatalf("BaseModelAndEffortForProvider(%q) = (%q, %q), want (%q, %q)", tc.model, base, effort, tc.wantBase, tc.wantEffort)
		}
		if got := ReasoningEffortsForProviderModel("opencode-go", tc.model); strings.Join(got, ",") != "high,max" {
			t.Fatalf("ReasoningEffortsForProviderModel(%q) = %v, want high/max", tc.model, got)
		}
	}
	if got := recorder.LastRequestForPathForTest(t, "/models").Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("models Authorization = %q, want bearer key", got)
	}
	if got := recorder.LastRequestForPathForTest(t, "/catalog").Header.Get("Authorization"); got != "" {
		t.Fatalf("catalog received subscription credential %q", got)
	}
}

func TestGetCachedOpenCodeGoModelsForAPIKeyReadsDiskCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	cacheKey := rp.OpenCodeGoCatalogScopeForTest("fresh-key", rp.OpenCodeGoBaseURLForTest)
	models := []rp.OpenCodeGoModelForTest{
		{ModelInfo: ModelInfo{ID: "active-model", ReasoningEfforts: []string{"low", "medium", "high"}}},
		{ModelInfo: ModelInfo{ID: "retired-model"}, Deprecated: true},
		{ModelInfo: ModelInfo{ID: ""}},
	}
	if err := rp.WriteCachedOpenCodeGoModelsForTest(cacheKey, models); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	// No provider has been constructed for this scope; completion must still
	// read the on-disk cache directly, and priming the scope must make its
	// reasoning efforts visible to the provider-aware effort helpers.
	got := GetCachedOpenCodeGoModelsForAPIKey("fresh-key")
	if strings.Join(got, ",") != "active-model" {
		t.Fatalf("GetCachedOpenCodeGoModelsForAPIKey(fresh-key) = %v, want [active-model]", got)
	}
	rp.PrimeOpenCodeGoCatalogScopeForTest("fresh-key", "")
	if efforts := ReasoningEffortsForProviderModel("opencode-go", "active-model"); strings.Join(efforts, ",") != "low,medium,high" {
		t.Fatalf("ReasoningEffortsForProviderModel(active-model) = %v, want low/medium/high", efforts)
	}

	if got := GetCachedOpenCodeGoModelsForAPIKey("other-key"); got != nil {
		t.Fatalf("GetCachedOpenCodeGoModelsForAPIKey(other-key) = %v, want nil", got)
	}
}

func TestOpenCodeGoCompletionRevealsCachedReasoningEfforts(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	const apiKey = "completion-key"
	cacheKey := rp.OpenCodeGoCatalogScopeForTest(apiKey, rp.OpenCodeGoBaseURLForTest)
	models := []rp.OpenCodeGoModelForTest{
		{ModelInfo: ModelInfo{ID: "gemini-model", ReasoningEfforts: []string{"low", "medium", "high"}}, Protocol: rp.OpenCodeGoProtocolGeminiForTest},
		{ModelInfo: ModelInfo{ID: "chat-model", ReasoningEfforts: []string{"high", "max"}}, Protocol: rp.OpenCodeGoProtocolChatForTest},
	}
	if err := rp.WriteCachedOpenCodeGoModelsForTest(cacheKey, models); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	providerCfg := config.ProviderConfig{Type: config.ProviderTypeOpenCodeGo, APIKey: apiKey}
	for name, cfg := range map[string]*config.Config{
		"cached models":     {Providers: map[string]config.ProviderConfig{"opencode-go": providerCfg}},
		"configured models": {Providers: map[string]config.ProviderConfig{"opencode-go": {Type: providerCfg.Type, APIKey: apiKey, Models: []string{"gemini-model", "chat-model"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			// Fresh-process completion: no provider has been constructed, so
			// effort variants must come from the primed disk cache, revealed
			// only once the parent model is fully typed.
			if got, want := GetProviderCompletions("opencode-go:gemini-model", false, cfg), []string{
				"opencode-go:gemini-model",
				"opencode-go:gemini-model-low",
				"opencode-go:gemini-model-medium",
				"opencode-go:gemini-model-high",
			}; !equalSlice(got, want) {
				t.Fatalf("full model completions = %v, want %v", got, want)
			}
			if got, want := GetProviderCompletions("opencode-go:gemini-m", false, cfg), []string{"opencode-go:gemini-model"}; !equalSlice(got, want) {
				t.Fatalf("partial model completions = %v, want %v", got, want)
			}
			if got, want := GetProviderCompletions("opencode-go:gemini-model-l", false, cfg), []string{"opencode-go:gemini-model-low"}; !equalSlice(got, want) {
				t.Fatalf("partial effort completions = %v, want %v", got, want)
			}
			if got := GetProviderCompletions("opencode-go:chat-model", false, cfg); !containsModelID(got, "opencode-go:chat-model-max") {
				t.Fatalf("chat model completions = %v, want max variant", got)
			}
		})
	}

	// A custom base_url scopes the cache by gateway: models cached for one
	// scope must not complete (or lend their efforts) under the other.
	const gatewayBase = "https://gateway.example.com/v1"
	gatewayModels := []rp.OpenCodeGoModelForTest{
		{ModelInfo: ModelInfo{ID: "gateway-model", ReasoningEfforts: []string{"low", "high"}}, Protocol: rp.OpenCodeGoProtocolGeminiForTest},
	}
	if err := rp.WriteCachedOpenCodeGoModelsForTest(rp.OpenCodeGoCatalogScopeForTest(apiKey, gatewayBase), gatewayModels); err != nil {
		t.Fatalf("write gateway cache: %v", err)
	}
	gatewayCfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"opencode-go": {Type: config.ProviderTypeOpenCodeGo, APIKey: apiKey, BaseURL: gatewayBase},
	}}
	if got, want := GetProviderCompletions("opencode-go:gateway-model", false, gatewayCfg), []string{
		"opencode-go:gateway-model",
		"opencode-go:gateway-model-low",
		"opencode-go:gateway-model-high",
	}; !equalSlice(got, want) {
		t.Fatalf("custom base completions = %v, want %v", got, want)
	}
	defaultCfg := &config.Config{Providers: map[string]config.ProviderConfig{"opencode-go": providerCfg}}
	if got := GetProviderCompletions("opencode-go:gateway-", false, defaultCfg); len(got) != 0 {
		t.Fatalf("default scope offered gateway-scoped models: %v", got)
	}
	if got := GetProviderCompletions("opencode-go:gemini-model", false, gatewayCfg); len(got) != 0 {
		t.Fatalf("gateway scope offered default-scoped models: %v", got)
	}
}

func TestRetryProviderForwardsOpenCodeGoFreshness(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, recorder := rp.NewOpenCodeGoTestServerForTest(t)
	defer server.Close()
	recorder.SetCatalogFailureForTest(true)
	wrapped := WrapWithRetry(rp.NewOpenCodeGoProviderForTest("test-key", "chat-model", server.URL, server.URL+"/catalog", server.Client()), RetryConfig{MaxAttempts: 1})
	lister, ok := wrapped.(interface {
		ListModelsWithFreshness(context.Context) ([]ModelInfo, bool, error)
	})
	if !ok {
		t.Fatal("retry provider does not expose model freshness")
	}
	models, fresh, err := lister.ListModelsWithFreshness(context.Background())
	if err != nil || len(models) == 0 || fresh {
		t.Fatalf("ListModelsWithFreshness = %d models, fresh %v, err %v", len(models), fresh, err)
	}
}

func TestConfigureContextManagementRefreshesOpenCodeGoMetadata(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, _ := rp.NewOpenCodeGoTestServerForTest(t)
	defer server.Close()
	provider := rp.NewOpenCodeGoProviderForTest("test-key", "responses-model", server.URL, server.URL+"/catalog", server.Client())
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())
	e := NewEngine(wrapped, nil)
	e.ConfigureContextManagement(wrapped, "opencode-go", "responses-model-high", true)
	if got := e.InputLimit(); got != 900 {
		t.Fatalf("InputLimit = %d, want 900", got)
	}
	if _, _, enabled := e.CompactionThresholds(); !enabled {
		t.Fatal("CompactionThresholds reports disabled compaction after metadata refresh")
	}
}

func TestZenListModelsIncludesReasoningEfforts(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-nano"},{"id":"qwen3.6-plus"},{"id":"big-pickle"},{"id":"mimo-v2.5-free"}]}`))
	}))
	defer modelsServer.Close()

	pricingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"opencode":{"models":{"gpt-5-nano":{"id":"gpt-5-nano","name":"GPT-5 Nano","cost":{"input":0.05,"output":0.4}},"qwen3.6-plus":{"id":"qwen3.6-plus","cost":{"input":1,"output":2}}}}}`))
	}))
	defer pricingServer.Close()

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"opencode":{"npm":"@ai-sdk/openai-compatible","api":"https://opencode.ai/zen/v1","models":{
			"gpt-5-nano":{"id":"gpt-5-nano","reasoning_options":[{"type":"effort","values":["minimal","low","medium","high"]}]},
			"qwen3.6-plus":{"id":"qwen3.6-plus","reasoning_options":[{"type":"toggle"},{"type":"budget_tokens","max":81920}]},
			"x-preview-f-free":{"id":"x-preview-f-free","reasoning_options":[{"type":"effort","values":["low","high","max"]}]}
		}}}`))
	}))
	defer catalogServer.Close()

	restoreModelsDevURL := (*rp.ModelsDevURLForTest)
	restoreCatalogURL := (*rp.ZenCatalogURLForTest)
	(*rp.ModelsDevURLForTest) = pricingServer.URL
	(*rp.ZenCatalogURLForTest) = catalogServer.URL
	defer func() {
		(*rp.ModelsDevURLForTest) = restoreModelsDevURL
		(*rp.ZenCatalogURLForTest) = restoreCatalogURL
	}()

	provider := &ZenProvider{OpenAICompatProvider: NewOpenAICompatProvider(modelsServer.URL, "", "", rp.ZenDisplayNameForTest)}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected only paid models, got %+v", models)
	}

	byID := make(map[string]ModelInfo, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}

	nano, ok := byID["gpt-5-nano"]
	if !ok {
		t.Fatalf("gpt-5-nano missing from results: %+v", models)
	}
	want := []string{"minimal", "low", "medium", "high"}
	if !equalSlice(nano.ReasoningEfforts, want) {
		t.Fatalf("gpt-5-nano efforts = %v, want %v", nano.ReasoningEfforts, want)
	}

	qwen, ok := byID["qwen3.6-plus"]
	if !ok {
		t.Fatalf("qwen3.6-plus missing from results: %+v", models)
	}
	// Toggle/budget reasoning options do not accept plain effort strings.
	if len(qwen.ReasoningEfforts) != 0 {
		t.Fatalf("qwen3.6-plus efforts = %v, want none", qwen.ReasoningEfforts)
	}

	got := GetProviderCompletions("zen:", false, nil)
	wantCompletions := []string{
		"zen:gpt-5-nano",
		"zen:gpt-5-nano-minimal",
		"zen:gpt-5-nano-low",
		"zen:gpt-5-nano-medium",
		"zen:gpt-5-nano-high",
		"zen:qwen3.6-plus",
	}
	if !equalSlice(got, wantCompletions) {
		t.Fatalf("Zen completions = %v, want live catalog %v", got, wantCompletions)
	}
	if containsModelID(got, "zen:mimo-v2.5-free") {
		t.Fatalf("Zen completions unexpectedly contain curated fallback: %v", got)
	}
}
