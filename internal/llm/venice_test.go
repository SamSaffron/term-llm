package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
