package responsesproviders

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
)

func TestEffectiveOpenCodeGoInputLimitPrefersExplicitInput(t *testing.T) {
	if got := effectiveOpenCodeGoInputLimit(1_050_000, 922_000, 128_000); got != 922_000 {
		t.Fatalf("explicit input limit = %d, want 922000", got)
	}
	if got := effectiveOpenCodeGoInputLimit(1_000, 0, 100); got != 900 {
		t.Fatalf("derived input limit = %d, want 900", got)
	}
}

func TestOpenCodeGoProtocolForMetadataGoogleFamily(t *testing.T) {
	for value, want := range map[string]opencodeGoProtocol{
		"@ai-sdk/google":                                       opencodeGoProtocolGemini,
		"@opencode/ai/providers/google":                        opencodeGoProtocolGemini,
		"@opencode/ai/providers/google/interactions":           opencodeGoProtocolGemini,
		"google-interactions":                                  opencodeGoProtocolGemini,
		"https://gateway.example.com/v1beta/models":            opencodeGoProtocolGemini,
		"https://gateway.example.com/models/x:generateContent": opencodeGoProtocolGemini,
		"@ai-sdk/openai":                                       opencodeGoProtocolResponses,
		"@ai-sdk/anthropic":                                    opencodeGoProtocolMessages,
		"@ai-sdk/openai-compatible":                            opencodeGoProtocolChat,
		"":                                                     opencodeGoProtocolChat,
	} {
		if got := openCodeGoProtocolForMetadata(value); got != want {
			t.Fatalf("openCodeGoProtocolForMetadata(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestUnknownOpenCodeGoModelProtocols(t *testing.T) {
	for id, want := range map[string]opencodeGoProtocol{
		"gemini-4-argon-contributor": opencodeGoProtocolGemini,
		"muse-spark-9.9-preview":     opencodeGoProtocolResponses,
		"some-other-model":           opencodeGoProtocolChat,
	} {
		if got := unknownOpenCodeGoModel(id).Protocol; got != want {
			t.Fatalf("unknownOpenCodeGoModel(%q).Protocol = %q, want %q", id, got, want)
		}
	}
}

func TestOpenCodeGoGeminiBaseURLTranslation(t *testing.T) {
	// The legacy zen/go shim 404s the Google route; only the default base
	// is translated to the inference base the catalog advertises.
	if got := openCodeGoGeminiBaseURL(opencodeGoBaseURL + "/"); got != opencodeGoInferenceBaseURL {
		t.Fatalf("default base translated to %q, want %q", got, opencodeGoInferenceBaseURL)
	}
	if got := openCodeGoGeminiBaseURL(opencodeGoInferenceBaseURL); got != opencodeGoInferenceBaseURL {
		t.Fatalf("inference base = %q, want unchanged", got)
	}
	if got := openCodeGoGeminiBaseURL("https://gateway.example.com/v1/"); got != "https://gateway.example.com/v1" {
		t.Fatalf("custom base = %q, want unchanged", got)
	}
	provider := NewOpenCodeGoProvider("test-key", "gemini-model")
	if want := opencodeGoInferenceBaseURL + "/models"; provider.gemini.BaseURL() != want {
		t.Fatalf("default Gemini baseURL = %q, want %q", provider.gemini.BaseURL(), want)
	}
}

func TestOpenCodeGoGeminiThinkingWithTools(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, recorder := newOpenCodeGoTestServer(t)
	defer server.Close()
	provider := newOpenCodeGoProvider("test-key", "gemini-model", server.URL, server.URL+"/catalog", server.Client())
	drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, provider, Request{
		Messages:        []Message{UserText("call live_probe")},
		Tools:           []ToolSpec{{Name: "live_probe", Description: "probe", Schema: map[string]any{"type": "object", "properties": map[string]any{"token": map[string]any{"type": "string"}}, "required": []string{"token"}}}},
		ReasoningEffort: "high",
		Search:          true,
	}))
	request := recorder.lastInferenceRequest(t)
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	generation, _ := body["generationConfig"].(map[string]any)
	thinking, _ := generation["thinkingConfig"].(map[string]any)
	if thinking["thinkingLevel"] != "HIGH" {
		t.Fatalf("thinking with tools = %#v, want HIGH level", body["generationConfig"])
	}
	if strings.Contains(string(request.Body), "googleSearch") {
		t.Fatalf("OpenCode Go Gemini route sent native search tool: %s", request.Body)
	}
}

func TestOpenCodeGoGeminiSendsThinkingBudgetForBudgetModels(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, recorder := newOpenCodeGoTestServer(t)
	defer server.Close()
	provider := newOpenCodeGoProvider("test-key", "gemini-budget-high", server.URL, server.URL+"/catalog", server.Client())
	provider.catalog.store(map[string]opencodeGoModel{
		"gemini-budget": {
			ModelInfo:        ModelInfo{ID: "gemini-budget", ReasoningEfforts: []string{"high", "max"}},
			Protocol:         opencodeGoProtocolGemini,
			ReasoningBudgets: map[string]int64{"high": 1234, "max": 31999},
			Available:        true,
		},
	}, time.Now(), time.Now(), nil)

	drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, provider, Request{Messages: []Message{UserText("hello")}}))
	request := recorder.lastRequestForPath(t, "/models/gemini-budget:streamGenerateContent")
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	generation, _ := body["generationConfig"].(map[string]any)
	thinking, _ := generation["thinkingConfig"].(map[string]any)
	if thinking["thinkingBudget"] != float64(1234) {
		t.Fatalf("thinkingBudget = %#v, want 1234 in %#v", thinking["thinkingBudget"], body)
	}
	if _, exists := thinking["thinkingLevel"]; exists {
		t.Fatalf("budget model also sent thinkingLevel: %#v", thinking)
	}
}

func TestOpenCodeGoGeminiRefusesRedirects(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var targetHits int
	var targetMu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetMu.Lock()
		targetHits++
		targetMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer gateway.Close()

	sharedClient := gateway.Client()
	provider := newOpenCodeGoProvider("test-key", "gemini-model", gateway.URL, gateway.URL+"/catalog", sharedClient)
	provider.catalog.store(map[string]opencodeGoModel{
		"gemini-model": {ModelInfo: ModelInfo{ID: "gemini-model"}, Protocol: opencodeGoProtocolGemini, Available: true},
	}, time.Now(), time.Now(), nil)

	stream := mustOpenCodeGoStream(t, provider, Request{Messages: []Message{UserText("hello")}})
	defer stream.Close()
	var sawError bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("redirected Gemini stream did not surface an error")
	}
	targetMu.Lock()
	defer targetMu.Unlock()
	if targetHits != 0 {
		t.Fatalf("redirect target received %d requests, want 0 (credential headers must not follow redirects)", targetHits)
	}
	if sharedClient.CheckRedirect != nil {
		t.Fatal("shared HTTP client redirect policy was mutated")
	}
}

func TestOpenCodeGoReasoningBudgetMatchesOpenCodeCap(t *testing.T) {
	metadata := opencodeGoCatalogModel{}
	metadata.Limit.Output = 262_144
	metadata.ReasoningOptions = append(metadata.ReasoningOptions, struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
		Min    int      `json:"min"`
		Max    int      `json:"max"`
	}{Type: "budget_tokens", Max: 262_144})

	efforts, budgets := openCodeGoReasoningMetadata(metadata)
	if strings.Join(efforts, ",") != "high,max" {
		t.Fatalf("efforts = %v, want high/max", efforts)
	}
	if budgets["high"] != 16_000 || budgets["max"] != 31_999 {
		t.Fatalf("budgets = %v, want high=16000 max=31999", budgets)
	}
}

func TestDecodeOpenCodeGoCatalogRejectsEmptyProvider(t *testing.T) {
	_, complete := decodeOpenCodeGoCatalog(struct {
		body []byte
		err  error
	}{body: []byte(`{"opencode-go":{"npm":"@ai-sdk/openai-compatible","models":{}}}`)})
	if complete {
		t.Fatal("empty OpenCode Go provider metadata reported complete")
	}
}

func TestOpenCodeGoCatalogUsesSingleFiveMinuteRefresh(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, recorder := newOpenCodeGoTestServer(t)
	defer server.Close()
	provider := newOpenCodeGoProvider("test-key", "chat-model", server.URL, server.URL+"/catalog", server.Client())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := provider.ListModels(context.Background()); err != nil {
				t.Errorf("ListModels: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := recorder.metadataRequestCount(); got != 2 {
		t.Fatalf("metadata requests = %d, want one request to each source", got)
	}
}

func TestOpenCodeGoFreshEmptyCacheTriggersRefresh(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	server, recorder := newOpenCodeGoTestServer(t)
	defer server.Close()
	provider := newOpenCodeGoProvider("test-key", "chat-model", server.URL, server.URL+"/catalog", server.Client())
	if err := cache.WriteModelCache(provider.catalog.cacheKey, nil); err != nil {
		t.Fatal(err)
	}
	models, err := provider.ListModels(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("ListModels = %d, %v; want refreshed models", len(models), err)
	}
	if got := recorder.metadataRequestCount(); got != 2 {
		t.Fatalf("metadata requests = %d, want refresh despite empty cache", got)
	}
}

func TestOpenCodeGoCatalogFailureUsesAvailabilityThenStaleMetadata(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	server, recorder := newOpenCodeGoTestServer(t)
	defer server.Close()

	// With no cache, a metadata outage still returns availability and safely
	// defaults inference to Chat Completions.
	recorder.setCatalogFailure(true)
	fallbackProvider := newOpenCodeGoProvider("test-key", "responses-model", server.URL, server.URL+"/catalog", server.Client())
	models, fresh, err := fallbackProvider.ListModelsWithFreshness(context.Background())
	if err != nil || len(models) != 8 || fresh {
		t.Fatalf("availability-only ListModels = %d, fresh %v, %v", len(models), fresh, err)
	}
	requestsBeforeStream := recorder.metadataRequestCount()
	drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, fallbackProvider, Request{Messages: []Message{UserText("hello")}}))
	if got := recorder.metadataRequestCount(); got != requestsBeforeStream {
		t.Fatalf("metadata requests after cached partial fallback = %d, want %d", got, requestsBeforeStream)
	}
	if got := recorder.lastInferenceRequest(t).Path; got != "/chat/completions" {
		t.Fatalf("availability-only route = %q, want chat fallback", got)
	}
	qwenFallback := newOpenCodeGoProvider("test-key", "qwen3.8-max", server.URL, server.URL+"/catalog", server.Client())
	drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, qwenFallback, Request{Messages: []Message{UserText("hello")}}))
	qwenRequest := recorder.lastInferenceRequest(t)
	var qwenBody map[string]any
	if err := json.Unmarshal(qwenRequest.Body, &qwenBody); err != nil {
		t.Fatal(err)
	}
	if qwenBody["model"] != "qwen3.8-max" {
		t.Fatalf("availability-only natural model ID = %#v, want qwen3.8-max", qwenBody["model"])
	}

	// Populate complete metadata, age both memory and disk, then fail the catalog
	// again. The stale protocol metadata must win over chat-only availability.
	recorder.setCatalogFailure(false)
	fallbackProvider.catalog.mu.Lock()
	fallbackProvider.catalog.lastAttempt = time.Now().Add(-time.Hour)
	fallbackProvider.catalog.mu.Unlock()
	completeProvider := newOpenCodeGoProvider("test-key", "messages-model", server.URL, server.URL+"/catalog", server.Client())
	if _, err := completeProvider.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	completeProvider.catalog.mu.Lock()
	completeProvider.catalog.fetchedAt = time.Now().Add(-time.Hour)
	completeProvider.catalog.mu.Unlock()
	cachePath := filepath.Join(cacheHome, "term-llm", completeProvider.catalog.cacheKey+"-models.json")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	disk["fetched_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	data, _ = json.Marshal(disk)
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	recorder.setCatalogFailure(true)
	drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, completeProvider, Request{Messages: []Message{UserText("hello")}}))
	if got := recorder.lastInferenceRequest(t).Path; got != "/v1/messages" {
		t.Fatalf("stale metadata route = %q, want messages", got)
	}
}

func TestOpenCodeGoCatalogFailureStripsEffortSuffix(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, recorder := newOpenCodeGoTestServer(t)
	defer server.Close()
	recorder.setCatalogFailure(true)
	provider := newOpenCodeGoProvider("test-key", "responses-model-high", server.URL, server.URL+"/catalog", server.Client())

	drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, provider, Request{Messages: []Message{UserText("hello")}}))
	request := recorder.lastInferenceRequest(t)
	if request.Path != "/chat/completions" {
		t.Fatalf("fallback path = %q, want chat completions", request.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "responses-model" {
		t.Fatalf("fallback model = %#v, want responses-model", body["model"])
	}
}

func TestOpenCodeGoCatalogIsScopedByCredential(t *testing.T) {
	server, _ := newOpenCodeGoTestServer(t)
	defer server.Close()
	first := newOpenCodeGoProvider("first-key", "chat-model", server.URL, server.URL+"/catalog", server.Client())
	second := newOpenCodeGoProvider("second-key", "chat-model", server.URL, server.URL+"/catalog", server.Client())
	if first.catalog == second.catalog || first.catalog.cacheKey == second.catalog.cacheKey {
		t.Fatal("providers with different credentials shared catalog state")
	}
}

func TestOpenCodeGoListModelsReturnsDefensiveCopies(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, _ := newOpenCodeGoTestServer(t)
	defer server.Close()
	provider := newOpenCodeGoProvider("test-key", "qwen3.8-max", server.URL, server.URL+"/catalog", server.Client())
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range models {
		if models[i].ID == "qwen3.8-max" {
			models[i].ReasoningEfforts[0] = "mutated"
		}
	}
	models, err = provider.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if model.ID == "qwen3.8-max" && strings.Join(model.ReasoningEfforts, ",") != "high,max" {
			t.Fatalf("cached reasoning efforts were mutated: %v", model.ReasoningEfforts)
		}
	}
}

func TestOpenCodeGoRoutesProtocolsAuthLimitsAndDeprecatedModel(t *testing.T) {
	for _, tc := range []struct {
		model         string
		wantPath      string
		wantHeader    string
		wantValue     string
		maxOutput     int
		reasoning     string
		wantMaxOutput float64
		wantThinking  bool
		wantBaseModel string
		wantLevel     string
	}{
		{model: "chat-model", wantPath: "/chat/completions", wantHeader: "Authorization", wantValue: "Bearer test-key"},
		{model: "messages-model", wantPath: "/v1/messages", wantHeader: "x-api-key", wantValue: "test-key"},
		{model: "qwen3.8-max", wantPath: "/v1/messages", wantHeader: "x-api-key", wantValue: "test-key", wantBaseModel: "qwen3.8-max"},
		{model: "qwen3.8-max-high", wantPath: "/v1/messages", wantHeader: "x-api-key", wantValue: "test-key", wantThinking: true, wantBaseModel: "qwen3.8-max"},
		{model: "responses-model", wantPath: "/responses", wantHeader: "Authorization", wantValue: "Bearer test-key"},
		{model: "muse-spark-1.2", wantPath: "/responses", wantHeader: "Authorization", wantValue: "Bearer test-key"},
		{model: "responses-model", reasoning: "none", wantPath: "/responses", wantHeader: "Authorization", wantValue: "Bearer test-key"},
		{model: "responses-model-high", wantPath: "/responses", wantHeader: "Authorization", wantValue: "Bearer test-key", wantBaseModel: "responses-model"},
		{model: "responses-model", wantPath: "/responses", wantHeader: "Authorization", wantValue: "Bearer test-key", maxOutput: 1000, wantMaxOutput: 100},
		{model: "gemini-model", wantPath: "/models/gemini-model:streamGenerateContent", wantHeader: "x-goog-api-key", wantValue: "test-key"},
		{model: "gemini-model", reasoning: "medium", wantPath: "/models/gemini-model:streamGenerateContent", wantHeader: "x-goog-api-key", wantValue: "test-key", wantLevel: "MEDIUM"},
		{model: "gemini-model-high", wantPath: "/models/gemini-model:streamGenerateContent", wantHeader: "x-goog-api-key", wantValue: "test-key", wantLevel: "HIGH"},
		{model: "unknown-preview", wantPath: "/chat/completions", wantHeader: "Authorization", wantValue: "Bearer test-key"},
		{model: "deprecated-model", wantPath: "/chat/completions", wantHeader: "Authorization", wantValue: "Bearer test-key"},
	} {
		t.Run(fmt.Sprintf("%s-%s-%d", tc.model, tc.reasoning, tc.maxOutput), func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			server, recorder := newOpenCodeGoTestServer(t)
			defer server.Close()
			provider := newOpenCodeGoProvider(" test-key\n", tc.model, server.URL, server.URL+"/catalog", server.Client())
			stream := mustOpenCodeGoStream(t, provider, Request{Messages: []Message{UserText("hello")}, MaxOutputTokens: tc.maxOutput, ReasoningEffort: tc.reasoning})
			drainOpenCodeGoStream(t, stream)
			request := recorder.lastInferenceRequest(t)
			if request.Path != tc.wantPath {
				t.Fatalf("path = %q, want %q", request.Path, tc.wantPath)
			}
			if request.Header.Get(tc.wantHeader) != tc.wantValue {
				t.Fatalf("%s = %q, want %q", tc.wantHeader, request.Header.Get(tc.wantHeader), tc.wantValue)
			}
			var body map[string]any
			if err := json.Unmarshal(request.Body, &body); err != nil {
				t.Fatal(err)
			}
			if tc.maxOutput == 0 {
				if _, exists := body["max_output_tokens"]; tc.wantPath == "/responses" && exists {
					t.Fatalf("unset max_output_tokens was expanded: %#v", body)
				}
				if _, exists := body["max_tokens"]; tc.wantPath == "/chat/completions" && exists {
					t.Fatalf("unset max_tokens was expanded: %#v", body)
				}
			}
			if tc.wantMaxOutput > 0 && body["max_output_tokens"] != tc.wantMaxOutput {
				t.Fatalf("max_output_tokens = %#v, want %g", body["max_output_tokens"], tc.wantMaxOutput)
			}
			if tc.wantBaseModel != "" && body["model"] != tc.wantBaseModel {
				t.Fatalf("model = %#v, want %q", body["model"], tc.wantBaseModel)
			}
			if tc.wantLevel != "" {
				generation, _ := body["generationConfig"].(map[string]any)
				thinking, _ := generation["thinkingConfig"].(map[string]any)
				if thinking["thinkingLevel"] != tc.wantLevel {
					t.Fatalf("thinkingLevel = %#v, want %q in %#v", thinking["thinkingLevel"], tc.wantLevel, body)
				}
			}
			if strings.HasSuffix(tc.wantPath, ":streamGenerateContent") && request.Header.Get("Authorization") != "Bearer test-key" {
				t.Fatalf("Gemini Authorization = %q, want bearer key", request.Header.Get("Authorization"))
			}
			if tc.wantThinking {
				thinking, ok := body["thinking"].(map[string]any)
				if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(50) {
					t.Fatalf("thinking = %#v, want enabled budget 50", body["thinking"])
				}
				if _, exists := body["output_config"]; exists {
					t.Fatalf("budget reasoning sent unsupported output_config: %#v", body["output_config"])
				}
			}
			if tc.wantPath == "/v1/messages" && body["max_tokens"] != float64(100) {
				t.Fatalf("Messages max_tokens = %#v, want catalog-clamped default 100", body["max_tokens"])
			}
			if tc.wantPath == "/responses" {
				if body["store"] != false {
					t.Fatalf("Responses store = %#v, want false", body["store"])
				}
				if tc.reasoning != "" {
					reasoning, ok := body["reasoning"].(map[string]any)
					if !ok || reasoning["effort"] != tc.reasoning {
						t.Fatalf("Responses reasoning = %#v, want effort %q", body["reasoning"], tc.reasoning)
					}
				}
				if _, ok := body["previous_response_id"]; ok {
					t.Fatalf("Responses request unexpectedly used server state: %#v", body)
				}
			}
		})
	}
}

func TestOpenCodeGoToolCallsAllProtocols(t *testing.T) {
	for _, model := range []string{"chat-model", "gemini-model", "messages-model", "responses-model"} {
		t.Run(model, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			server, _ := newOpenCodeGoTestServer(t)
			defer server.Close()
			provider := newOpenCodeGoProvider("test-key", model, server.URL, server.URL+"/catalog", server.Client())
			stream := mustOpenCodeGoStream(t, provider, Request{
				Messages:   []Message{UserText("call live_probe")},
				Tools:      []ToolSpec{{Name: "live_probe", Description: "probe", Schema: map[string]any{"type": "object", "properties": map[string]any{"token": map[string]any{"type": "string"}}, "required": []string{"token"}}}},
				ToolChoice: ToolChoice{Mode: ToolChoiceName, Name: "live_probe"},
			})
			var call *ToolCall
			for {
				event, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if event.Type == EventToolCall && event.Tool != nil {
					call = event.Tool
				}
			}
			_ = stream.Close()
			if call == nil || call.Name != "live_probe" || !strings.Contains(string(call.Arguments), "TOOL_OK") {
				t.Fatalf("tool call = %#v", call)
			}
		})
	}
}

func TestOpenCodeGoSendsSessionAttributionHeaders(t *testing.T) {
	for _, tc := range []struct {
		model    string
		wantPath string
	}{
		{model: "chat-model", wantPath: "/chat/completions"},
		{model: "gemini-model", wantPath: "/models/gemini-model:streamGenerateContent"},
		{model: "messages-model", wantPath: "/v1/messages"},
		{model: "responses-model", wantPath: "/responses"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			server, recorder := newOpenCodeGoTestServer(t)
			defer server.Close()
			provider := newOpenCodeGoProvider("test-key", tc.model, server.URL, server.URL+"/catalog", server.Client())

			drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, provider, Request{
				SessionID: "session-abc",
				Messages:  []Message{UserText("hello")},
			}))
			request := recorder.lastInferenceRequest(t)
			if request.Path != tc.wantPath {
				t.Fatalf("path = %q, want %q", request.Path, tc.wantPath)
			}
			if got := request.Header.Get(openCodeGoSessionHeader); got != "session-abc" {
				t.Fatalf("%s = %q, want session-abc", openCodeGoSessionHeader, got)
			}
			if got := request.Header.Get(openCodeGoClientHeader); got != openCodeGoClientID {
				t.Fatalf("%s = %q, want %q", openCodeGoClientHeader, got, openCodeGoClientID)
			}

			// Requests without a session ID still carry one, stable for this process.
			drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, provider, Request{Messages: []Message{UserText("hello")}}))
			first := recorder.lastInferenceRequest(t).Header.Get(openCodeGoSessionHeader)
			if !validOpenCodeGoSessionID(first) {
				t.Fatalf("fallback session = %q, want a header-safe ID", first)
			}
			drainOpenCodeGoStream(t, mustOpenCodeGoStream(t, provider, Request{
				SessionID: "bad session\nid",
				Messages:  []Message{UserText("hello")},
			}))
			if second := recorder.lastInferenceRequest(t).Header.Get(openCodeGoSessionHeader); second != first {
				t.Fatalf("unsafe session ID = %q, want stable fallback %q", second, first)
			}
		})
	}
}

func TestOpenCodeGoCatalogSendsAttributionHeadersToLiveEndpointOnly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server, recorder := newOpenCodeGoTestServer(t)
	defer server.Close()
	provider := newOpenCodeGoProvider("test-key", "chat-model", server.URL, server.URL+"/catalog", server.Client())
	if _, err := provider.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	live := recorder.lastRequestForPath(t, "/models")
	if got := live.Header.Get(openCodeGoClientHeader); got != openCodeGoClientID {
		t.Fatalf("live catalog %s = %q, want %q", openCodeGoClientHeader, got, openCodeGoClientID)
	}
	if got := live.Header.Get(openCodeGoSessionHeader); !validOpenCodeGoSessionID(got) {
		t.Fatalf("live catalog %s = %q, want a header-safe fallback ID", openCodeGoSessionHeader, got)
	}
	mirror := recorder.lastRequestForPath(t, "/catalog")
	if got := mirror.Header.Get(openCodeGoClientHeader); got != "" {
		t.Fatalf("public catalog mirror %s = %q, want unset", openCodeGoClientHeader, got)
	}
	if got := mirror.Header.Get(openCodeGoSessionHeader); got != "" {
		t.Fatalf("public catalog mirror %s = %q, want unset", openCodeGoSessionHeader, got)
	}

	// Model listing is public, but the live endpoint is still OpenCode's API.
	unauthenticated := newOpenCodeGoProvider("", "chat-model", server.URL, server.URL+"/catalog", server.Client())
	if _, err := unauthenticated.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	publicLive := recorder.lastRequestForPath(t, "/models")
	if got := publicLive.Header.Get("Authorization"); got != "" {
		t.Fatalf("unauthenticated live catalog Authorization = %q, want unset", got)
	}
	if got := publicLive.Header.Get(openCodeGoClientHeader); got != openCodeGoClientID {
		t.Fatalf("unauthenticated live catalog %s = %q, want %q", openCodeGoClientHeader, got, openCodeGoClientID)
	}
	if got := publicLive.Header.Get(openCodeGoSessionHeader); got != live.Header.Get(openCodeGoSessionHeader) {
		t.Fatalf("unauthenticated live catalog %s = %q, want fallback %q", openCodeGoSessionHeader, got, live.Header.Get(openCodeGoSessionHeader))
	}
}

func mustOpenCodeGoStream(t *testing.T, provider *OpenCodeGoProvider, req Request) Stream {
	t.Helper()
	stream, err := provider.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return stream
}

func drainOpenCodeGoStream(t *testing.T, stream Stream) {
	t.Helper()
	defer stream.Close()
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}
}

type openCodeGoRecordedRequest struct {
	Path   string
	Header http.Header
	Body   []byte
}

type openCodeGoTestRecorder struct {
	mu          sync.Mutex
	requests    []openCodeGoRecordedRequest
	failCatalog bool
}

func (r *openCodeGoTestRecorder) record(req *http.Request) []byte {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.requests = append(r.requests, openCodeGoRecordedRequest{Path: req.URL.Path, Header: req.Header.Clone(), Body: body})
	r.mu.Unlock()
	return body
}

func (r *openCodeGoTestRecorder) setCatalogFailure(fail bool) {
	r.mu.Lock()
	r.failCatalog = fail
	r.mu.Unlock()
}

func (r *openCodeGoTestRecorder) catalogFailure() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failCatalog
}

func (r *openCodeGoTestRecorder) metadataRequestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, request := range r.requests {
		if request.Path == "/models" || request.Path == "/catalog" {
			count++
		}
	}
	return count
}

func (r *openCodeGoTestRecorder) lastRequestForPath(t *testing.T, path string) openCodeGoRecordedRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if r.requests[i].Path == path {
			return r.requests[i]
		}
	}
	t.Fatalf("no request recorded for %s", path)
	return openCodeGoRecordedRequest{}
}

func (r *openCodeGoTestRecorder) lastInferenceRequest(t *testing.T) openCodeGoRecordedRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		switch r.requests[i].Path {
		case "/chat/completions", "/v1/messages", "/responses", "/models/gemini-model:streamGenerateContent":
			return r.requests[i]
		}
	}
	t.Fatal("no inference request recorded")
	return openCodeGoRecordedRequest{}
}

func newOpenCodeGoTestServer(t *testing.T) (*httptest.Server, *openCodeGoTestRecorder) {
	t.Helper()
	recorder := &openCodeGoTestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := recorder.record(r)
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"chat-model"},{"id":"gemini-model"},{"id":"messages-model"},{"id":"muse-spark-1.2"},{"id":"qwen3.8-max"},{"id":"responses-model"},{"id":"deprecated-model"},{"id":"unknown-preview"}]}`)
		case "/catalog":
			if recorder.catalogFailure() {
				http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"opencode-go":{"npm":"@ai-sdk/openai-compatible","models":{"chat-model":{"id":"chat-model","name":"Chat Model","limit":{"context":1000,"output":100},"cost":{"input":0.5,"output":1}},"gemini-model":{"id":"gemini-model","name":"Gemini Model","provider":{"npm":"@ai-sdk/google"},"limit":{"context":1000,"output":100},"cost":{"input":4,"output":20},"reasoning_options":[{"type":"effort","values":["low","medium","high"]}]},"messages-model":{"id":"messages-model","name":"Messages Model","provider":{"npm":"@ai-sdk/anthropic"},"limit":{"context":1000,"output":100}},"qwen3.8-max":{"id":"qwen3.8-max","name":"Qwen3.8 Max","provider":{"npm":"@ai-sdk/anthropic"},"limit":{"context":1000,"output":100},"reasoning_options":[{"type":"toggle"},{"type":"budget_tokens","max":250}]},"responses-model":{"id":"responses-model","name":"Responses Model","provider":{"npm":"@ai-sdk/openai"},"limit":{"context":1000,"output":100},"cost":{"input":1.25,"output":5},"reasoning_options":[{"type":"effort","values":["low","high"]}]},"deprecated-model":{"id":"deprecated-model","status":"deprecated"}}}}`)
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			if strings.Contains(string(body), `"tools"`) {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_chat\",\"type\":\"function\",\"function\":{\"name\":\"live_probe\",\"arguments\":\"{\\\"token\\\":\\\"TOOL_OK\\\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			} else {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chat\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			if strings.Contains(string(body), `"tools"`) {
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"messages-model\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_messages\",\"name\":\"live_probe\",\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"token\\\":\\\"TOOL_OK\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			} else {
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"messages-model\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"messages\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}
		case "/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			if strings.Contains(string(body), `"tools"`) {
				fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_responses\",\"name\":\"live_probe\",\"arguments\":\"\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"{\\\"token\\\":\\\"TOOL_OK\\\"}\"}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_responses\",\"name\":\"live_probe\",\"arguments\":\"{\\\"token\\\":\\\"TOOL_OK\\\"}\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			} else {
				fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"responses\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			}
		case "/models/gemini-model:streamGenerateContent":
			w.Header().Set("Content-Type", "text/event-stream")
			if strings.Contains(string(body), `"functionDeclarations"`) {
				fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"id\":\"call_gemini\",\"name\":\"live_probe\",\"args\":{\"token\":\"TOOL_OK\"}}}]}}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\ndata: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n")
			} else {
				fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"gemini\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\ndata: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n")
			}
		default:
			if strings.HasPrefix(r.URL.Path, "/models/") && strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"gemini\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\ndata: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\"}]}}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n")
				return
			}
			http.NotFound(w, r)
		}
	}))
	return server, recorder
}
