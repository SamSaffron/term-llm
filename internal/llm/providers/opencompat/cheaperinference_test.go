package opencompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewCheaperInferenceProviderNormalizesAPIKey(t *testing.T) {
	for _, raw := range []string{"  test-key\n", "  Bearer test-key\n"} {
		provider := NewCheaperInferenceProvider(raw, "")
		if provider.apiKey != "test-key" {
			t.Fatalf("apiKey from %q = %q, want %q", raw, provider.apiKey, "test-key")
		}
	}
}

func TestNewCheaperInferenceProviderDefaultsModel(t *testing.T) {
	provider := NewCheaperInferenceProvider("k", "")
	if provider.model != "gpt-5.4-mini" {
		t.Fatalf("model = %q, want gpt-5.4-mini", provider.model)
	}
	if provider.baseURL != "https://api.cheaperinference.com/v1" {
		t.Fatalf("baseURL = %q, want https://api.cheaperinference.com/v1", provider.baseURL)
	}
}

func TestCheaperInferenceProviderCapabilities(t *testing.T) {
	caps := NewCheaperInferenceProvider("key", "").Capabilities()
	if caps.NativeWebSearch || caps.NativeWebFetch {
		t.Fatalf("expected no native web tools, got %+v", caps)
	}
	if !caps.ToolCalls || !caps.SupportsToolChoice {
		t.Fatalf("expected tool calls with tool_choice, got %+v", caps)
	}
}

func TestCheaperInferenceListModelsSendsAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4-mini","object":"model","owned_by":"openai"}]}`))
	}))
	defer srv.Close()

	provider := &CheaperInferenceProvider{OpenAICompatProvider: NewOpenAICompatProvider(srv.URL, "key", "", "Cheaper Inference")}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.4-mini" {
		t.Fatalf("models = %+v, want one gpt-5.4-mini entry", models)
	}
}
