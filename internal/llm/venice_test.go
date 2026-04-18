package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

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

func TestVeniceProviderListModelsUsesProviderScopedLimits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-code-fast-1","object":"model","created":1,"owned_by":"venice.ai"},{"id":"minimax-m27","object":"model","created":1,"owned_by":"venice.ai"}]}`))
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
	if models[0].InputLimit != 256_000 {
		t.Fatalf("grok-code-fast-1 InputLimit = %d, want 256000", models[0].InputLimit)
	}
	if models[1].InputLimit != 198_000 {
		t.Fatalf("minimax-m27 InputLimit = %d, want 198000", models[1].InputLimit)
	}
}

func TestParseVeniceModelSuffix(t *testing.T) {
	base, params := parseVeniceModelSuffix("grok-4-20-beta:enable_x_search=true&enable_web_citations=true")
	if base != "grok-4-20-beta" {
		t.Fatalf("base model = %q, want grok-4-20-beta", base)
	}
	if params["enable_x_search"] != true {
		t.Fatalf("expected enable_x_search=true, got %#v", params["enable_x_search"])
	}
	if params["enable_web_citations"] != true {
		t.Fatalf("expected enable_web_citations=true, got %#v", params["enable_web_citations"])
	}
}

func TestBuildVeniceModelAndParams_PreservesExplicitXSearch(t *testing.T) {
	model, params := buildVeniceModelAndParams("grok-4-20-beta:enable_x_search=true", true)
	if model != "grok-4-20-beta" {
		t.Fatalf("model = %q, want grok-4-20-beta", model)
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

			actualModel, effort := parseModelEffort(tt.providerModel)
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
		Model:    "grok-4-20-beta:enable_x_search=true",
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

	if got.Model != "grok-4-20-beta" {
		t.Fatalf("expected stripped base model, got %q", got.Model)
	}
	if got.VeniceParameters["enable_x_search"] != true {
		t.Fatalf("expected venice_parameters.enable_x_search=true, got %#v", got.VeniceParameters)
	}
	if _, ok := got.VeniceParameters["enable_web_search"]; ok {
		t.Fatalf("did not expect enable_web_search alongside explicit x search: %#v", got.VeniceParameters)
	}
}
