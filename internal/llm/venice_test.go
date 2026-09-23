package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewProviderByName_ResolvesConfiguredVeniceCredentialsOnDemand(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "")

	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"venice": {
				APIKey: "  test-key\n",
				Model:  "venice-uncensored",
			},
		},
	}

	provider, err := NewProviderByName(cfg, "venice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider == nil {
		t.Fatal("expected provider")
	}
	if got := strings.TrimSpace(cfg.Providers["venice"].ResolvedAPIKey); got != "test-key" {
		t.Fatalf("ResolvedAPIKey = %q, want %q", got, "test-key")
	}
}

func TestNewVeniceProviderTrimsAPIKey(t *testing.T) {
	provider := NewVeniceProvider("  test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewVeniceProviderStripsBearerPrefix(t *testing.T) {
	provider := NewVeniceProvider("  Bearer test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestCreateProviderFromConfig_VeniceRequiresAPIKey(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "")

	_, err := createProviderFromConfig("venice", &config.ProviderConfig{Model: "venice-uncensored"})
	if err == nil {
		t.Fatal("expected missing Venice API key to return an error")
	}
	if !strings.Contains(err.Error(), "VENICE_API_KEY") {
		t.Fatalf("expected VENICE_API_KEY guidance, got %v", err)
	}
}

func TestCreateProviderFromConfig_VeniceTrimsConfiguredAPIKey(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "")

	provider, err := createProviderFromConfig("venice", &config.ProviderConfig{
		ResolvedAPIKey: "  test-key\n",
		Model:          "venice-uncensored",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	venice, ok := provider.(*VeniceProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *VeniceProvider", provider)
	}
	if venice.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", venice.apiKey, "test-key")
	}
}

func TestVeniceListModelsKeepsLiveContextLengthForUncuratedModels(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"brand-new-venice-model","created":123,"owned_by":"venice.ai","context_length":777000}]}`)
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "", "Venice")}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models len = %d, want 1", len(models))
	}
	if models[0].InputLimit != 777000 {
		t.Fatalf("InputLimit = %d, want live context_length", models[0].InputLimit)
	}
	if got := InputLimitForProviderModel("venice", "brand-new-venice-model"); got != 777000 {
		t.Fatalf("cached InputLimitForProviderModel = %d, want 777000", got)
	}
}

func TestVeniceStream_SuppressesLeadingReasoningWhitespaceArtifact(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"\\n\\n\",\"reasoning\":\"thinking\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var gotText, gotReasoning string
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			gotText += event.Text
		case EventReasoningDelta:
			gotReasoning += event.Text
		case EventError:
			t.Fatalf("unexpected error event: %v", event.Err)
		}
	}

	if gotText != "hello" {
		t.Fatalf("text = %q, want reasoning whitespace artifact suppressed", gotText)
	}
	if gotReasoning != "thinking" {
		t.Fatalf("reasoning = %q, want preserved reasoning", gotReasoning)
	}
}

func TestVeniceStream_ParsesInlineThinkTagsAsReasoning(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"<thi\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"nk>The user said hi</think>\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"Hello!\"}}]}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var gotText, gotReasoning string
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			gotText += event.Text
		case EventReasoningDelta:
			gotReasoning += event.Text
		case EventError:
			t.Fatalf("unexpected error event: %v", event.Err)
		}
	}

	if gotText != "Hello!" {
		t.Fatalf("text = %q, want inline think stripped from visible text", gotText)
	}
	if gotReasoning != "The user said hi" {
		t.Fatalf("reasoning = %q, want inline think body", gotReasoning)
	}
}

func TestVeniceStream_DoesNotParseInlineThinkTagsAfterVisibleText(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"Use <think>literal</think> tags\"}}]}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var gotText, gotReasoning string
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			gotText += event.Text
		case EventReasoningDelta:
			gotReasoning += event.Text
		}
	}

	if gotText != "Use <think>literal</think> tags" {
		t.Fatalf("text = %q, want literal tags preserved", gotText)
	}
	if gotReasoning != "" {
		t.Fatalf("reasoning = %q, want none", gotReasoning)
	}
}

func TestVeniceProviderCapabilities(t *testing.T) {
	provider := NewVeniceProvider("key", "")
	caps := provider.Capabilities()
	if !caps.NativeWebSearch {
		t.Fatal("expected NativeWebSearch=true")
	}
	if caps.NativeWebFetch {
		t.Fatal("expected NativeWebFetch=false")
	}
	if !caps.ToolCalls {
		t.Fatal("expected ToolCalls=true")
	}
	if !caps.SupportsToolChoice {
		t.Fatal("expected SupportsToolChoice=true")
	}
}

func TestVeniceProviderListModelsDoesNotUseStaticLimitsWhenContextLengthMissing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4-20","object":"model","created":1,"owned_by":"venice.ai"},{"id":"minimax-m27","object":"model","created":1,"owned_by":"venice.ai"}]}`))
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("ListModels() returned %d models, want 2", len(models))
	}
	for _, model := range models {
		if model.InputLimit != 0 {
			t.Fatalf("%s InputLimit = %d, want 0 when Venice omits context_length", model.ID, model.InputLimit)
		}
	}
}

func TestParseVeniceModelSuffix(t *testing.T) {
	base, params := parseVeniceModelSuffix("grok-4-20:enable_x_search=true&enable_web_citations=true")
	if base != "grok-4-20" {
		t.Fatalf("base model = %q, want grok-4-20", base)
	}
	if params["enable_x_search"] != true {
		t.Fatalf("expected enable_x_search=true, got %#v", params["enable_x_search"])
	}
	if params["enable_web_citations"] != true {
		t.Fatalf("expected enable_web_citations=true, got %#v", params["enable_web_citations"])
	}
}

func TestBuildVeniceModelAndParams_PreservesExplicitXSearch(t *testing.T) {
	model, params := buildVeniceModelAndParams("grok-4-20:enable_x_search=true", true)
	if model != "grok-4-20" {
		t.Fatalf("model = %q, want grok-4-20", model)
	}
	if params["enable_x_search"] != true {
		t.Fatalf("expected enable_x_search=true, got %#v", params["enable_x_search"])
	}
	if _, ok := params["enable_web_search"]; ok {
		t.Fatalf("did not expect enable_web_search when explicit x search is set: %#v", params)
	}
}

func TestBuildVeniceModelAndParams_AddsWebSearchWhenNeeded(t *testing.T) {
	model, params := buildVeniceModelAndParams("venice-uncensored", true)
	if model != "venice-uncensored" {
		t.Fatalf("model = %q, want venice-uncensored", model)
	}
	if params["enable_web_search"] != "on" {
		t.Fatalf("expected enable_web_search=on, got %#v", params["enable_web_search"])
	}
}

func TestVeniceProviderSearchUsesVeniceParametersAndBaseModel(t *testing.T) {
	var got struct {
		Model             string                 `json:"model"`
		ParallelToolCalls *bool                  `json:"parallel_tool_calls,omitempty"`
		Stream            bool                   `json:"stream"`
		VeniceParameters  map[string]interface{} `json:"venice_parameters,omitempty"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	stream, err := provider.Stream(context.Background(), Request{
		Messages:          []Message{UserText("latest Venice news")},
		Search:            true,
		ParallelToolCalls: true,
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

	if got.Model != "venice-uncensored" {
		t.Fatalf("expected base model only, got %q", got.Model)
	}
	if got.VeniceParameters["enable_web_search"] != "on" {
		t.Fatalf("expected venice_parameters.enable_web_search=on, got %#v", got.VeniceParameters)
	}
	if got.ParallelToolCalls != nil {
		t.Fatalf("expected parallel_tool_calls to be omitted/false, got %+v", got.ParallelToolCalls)
	}
	if !got.Stream {
		t.Fatal("expected stream=true")
	}
}

func TestVeniceStreamDeltaCompletionAndErrors(t *testing.T) {
	tests := []struct {
		name      string
		wire      string
		wantTypes []EventType
		wantText  string
		wantError string
	}{
		{
			name: "tool and usage after malformed data",
			wire: "data: not-json\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"id\":"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":2}}}` + "\n\n" +
				"data: [DONE]\n\n",
			wantTypes: []EventType{EventToolCall, EventUsage, EventDone},
		},
		{
			name:      "flush unfinished inline tag at EOF",
			wire:      "data: {\"choices\":[{\"delta\":{\"content\":\"<thi\"}}]}\n\n",
			wantTypes: []EventType{EventTextDelta, EventDone},
			wantText:  "<thi",
		},
		{
			name:      "structured error event",
			wire:      "event: error\ndata: {\"error\":{\"message\":\"quota exceeded\"}}\n\n",
			wantError: "quota exceeded",
		},
		{
			name:      "error after incomplete tool call",
			wire:      `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}` + "\n\n" + "data: [DONE]\n\n",
			wantError: "missing tool name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			}))
			defer ts.Close()
			provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
			stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var gotTypes []EventType
			var gotText string
			var gotError error
			for {
				event, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					gotError = err
					break
				}
				if event.Type == EventError {
					gotError = event.Err
					break
				}
				gotTypes = append(gotTypes, event.Type)
				gotText += event.Text
				if event.Type == EventToolCall && (event.Tool.ID != "call_1" || event.Tool.Name != "lookup" || string(event.Tool.Arguments) != `{"id":1}`) {
					t.Errorf("tool call = %+v", event.Tool)
				}
				if event.Type == EventUsage && (event.Use.InputTokens != 7 || event.Use.CachedInputTokens != 2 || event.Use.OutputTokens != 3) {
					t.Errorf("usage = %+v", event.Use)
				}
			}
			if tc.wantError != "" {
				if gotError == nil || !strings.Contains(gotError.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %q", gotError, tc.wantError)
				}
				return
			}
			if gotError != nil {
				t.Fatal(gotError)
			}
			if !reflect.DeepEqual(gotTypes, tc.wantTypes) || gotText != tc.wantText {
				t.Fatalf("events = %v, text = %q; want %v, %q", gotTypes, gotText, tc.wantTypes, tc.wantText)
			}
		})
	}
}

func TestVeniceProviderPlainTextErrorEventSurfacesError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\ndata: Service temporarily unavailable.\n\n"))
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var recvErr error
	for {
		ev, err := stream.Recv()
		if err != nil {
			recvErr = err
			break
		}
		if ev.Type == EventError {
			recvErr = ev.Err
			break
		}
		if ev.Type == EventDone {
			break
		}
	}
	if recvErr == nil {
		t.Fatal("expected error from plain-text error event, got none")
	}
	if !strings.Contains(recvErr.Error(), "Service temporarily unavailable") {
		t.Fatalf("expected 'Service temporarily unavailable' in error, got: %v", recvErr)
	}
}

func TestVeniceProviderReasoningEffortPrecedence(t *testing.T) {
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
			providerModel: "venice-uncensored-high",
			wantEffort:    "high",
		},
		{
			name:           "request suffix overrides provider effort",
			providerModel:  "venice-uncensored",
			providerEffort: "low",
			requestModel:   "venice-uncensored-high",
			wantEffort:     "high",
		},
		{
			name:           "request reasoning_effort field wins over provider effort and suffix",
			providerModel:  "venice-uncensored",
			providerEffort: "low",
			requestModel:   "venice-uncensored-medium",
			requestEffort:  "high",
			wantEffort:     "high",
		},
		{
			name:          "minimal effort passes through",
			providerModel: "venice-uncensored",
			requestEffort: "minimal",
			wantEffort:    "minimal",
		},
		{
			name:          "max suffix on request model passes through",
			providerModel: "venice-uncensored",
			requestModel:  "claude-opus-4-7-max",
			wantEffort:    "max",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				ReasoningEffort string `json:"reasoning_effort"`
			}

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					t.Fatalf("unexpected path %q", r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer ts.Close()

			actualModel, effort := ParseModelEffort(tt.providerModel)
			provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", actualModel, "Venice")}
			provider.effort = effort
			if tt.providerEffort != "" {
				provider.effort = tt.providerEffort
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

			if got.ReasoningEffort != tt.wantEffort {
				t.Errorf("reasoning_effort = %q, want %q", got.ReasoningEffort, tt.wantEffort)
			}
		})
	}
}

func TestVeniceProviderExplicitXSearchUsesVeniceParameters(t *testing.T) {
	var got struct {
		Model            string                 `json:"model"`
		VeniceParameters map[string]interface{} `json:"venice_parameters,omitempty"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("find recent posts")},
		Search:   true,
		Model:    "grok-4-20:enable_x_search=true",
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

	if got.Model != "grok-4-20" {
		t.Fatalf("expected stripped base model, got %q", got.Model)
	}
	if got.VeniceParameters["enable_x_search"] != true {
		t.Fatalf("expected venice_parameters.enable_x_search=true, got %#v", got.VeniceParameters)
	}
	if _, ok := got.VeniceParameters["enable_web_search"]; ok {
		t.Fatalf("did not expect enable_web_search alongside explicit x search: %#v", got.VeniceParameters)
	}
}

func TestVeniceProvider429IsRetried(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"The model is currently overloaded. Please try again later."}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	inner := &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "venice-uncensored", "Venice")}
	provider := WrapWithRetry(inner, RetryConfig{MaxAttempts: 5, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond})

	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var text string
	for {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Recv() error = %v", err)
		}
		if ev.Type == EventError {
			t.Fatalf("unexpected EventError after retry: %v", ev.Err)
		}
		if ev.Type == EventDone {
			break
		}
		if ev.Type == EventTextDelta {
			text += ev.Text
		}
	}

	if callCount != 3 {
		t.Fatalf("callCount = %d, want 3 (2 × 429 then success)", callCount)
	}
	if text != "ok" {
		t.Fatalf("text = %q, want %q", text, "ok")
	}
}
