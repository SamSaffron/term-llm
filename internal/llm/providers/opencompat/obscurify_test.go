package opencompat

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewObscurifyProviderNormalizesAPIKey(t *testing.T) {
	for _, raw := range []string{"  test-key\n", "  Bearer test-key\n"} {
		provider := NewObscurifyProvider(raw, "")
		if provider.apiKey != "test-key" {
			t.Fatalf("apiKey from %q = %q, want %q", raw, provider.apiKey, "test-key")
		}
	}
}

func TestNewObscurifyProviderDefaultsModel(t *testing.T) {
	provider := NewObscurifyProvider("k", "")
	if provider.model != "z-ai/glm-5.3" {
		t.Fatalf("model = %q, want z-ai/glm-5.3", provider.model)
	}
	if provider.baseURL != "https://obscurify.ai/v1" {
		t.Fatalf("baseURL = %q, want https://obscurify.ai/v1", provider.baseURL)
	}
}

func TestObscurifyProviderCapabilities(t *testing.T) {
	caps := NewObscurifyProvider("key", "").Capabilities()
	if caps.NativeWebSearch || caps.NativeWebFetch {
		t.Fatalf("expected no native web tools, got %+v", caps)
	}
	if !caps.ToolCalls || !caps.SupportsToolChoice {
		t.Fatalf("expected tool calls with tool_choice, got %+v", caps)
	}
}

func TestObscurifyListModelsReadsContextAndPricing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"example/model-a","name":"Example: Model A","pricing":{"prompt":2e-07,"completion":8e-07},"context_length":131072,"tools":true}]}`))
	}))
	defer srv.Close()

	provider := &ObscurifyProvider{OpenAICompatProvider: NewOpenAICompatProvider(srv.URL, "key", "", "Obscurify.ai")}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	m := models[0]
	if m.ID != "example/model-a" || m.InputLimit != 131072 {
		t.Fatalf("model = %+v, want example/model-a with 131072 input limit", m)
	}
	if math.Abs(m.InputPrice-0.2) > 1e-9 || math.Abs(m.OutputPrice-0.8) > 1e-9 {
		t.Fatalf("prices = %v/%v per 1M, want 0.2/0.8", m.InputPrice, m.OutputPrice)
	}
}
