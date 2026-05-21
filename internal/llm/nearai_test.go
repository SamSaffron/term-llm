package llm

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewProviderByName_ResolvesConfiguredNearAICredentialsOnDemand(t *testing.T) {
	t.Setenv("NEARAI_API_KEY", "")

	cfg := &config.Config{
		DefaultProvider: "openai",
		Providers: map[string]config.ProviderConfig{
			"nearai": {
				APIKey: "  test-key\n",
				Model:  "zai-org/GLM-5.1-FP8",
			},
		},
	}

	provider, err := NewProviderByName(cfg, "nearai", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider == nil {
		t.Fatal("expected provider")
	}
	if got := strings.TrimSpace(cfg.Providers["nearai"].ResolvedAPIKey); got != "test-key" {
		t.Fatalf("ResolvedAPIKey = %q, want %q", got, "test-key")
	}
}

func TestNewNearAIProviderTrimsAPIKey(t *testing.T) {
	provider := NewNearAIProvider("  test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewNearAIProviderStripsBearerPrefix(t *testing.T) {
	provider := NewNearAIProvider("  Bearer test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewNearAIProviderDefaultsModel(t *testing.T) {
	provider := NewNearAIProvider("k", "")
	if provider.model != "zai-org/GLM-5.1-FP8" {
		t.Fatalf("model = %q, want zai-org/GLM-5.1-FP8", provider.model)
	}
}

func TestCreateProviderFromConfig_NearAIRequiresAPIKey(t *testing.T) {
	t.Setenv("NEARAI_API_KEY", "")

	_, err := createProviderFromConfig("nearai", &config.ProviderConfig{Model: "zai-org/GLM-5.1-FP8"})
	if err == nil {
		t.Fatal("expected missing NEAR AI API key to return an error")
	}
	if !strings.Contains(err.Error(), "NEARAI_API_KEY") {
		t.Fatalf("expected NEARAI_API_KEY guidance, got %v", err)
	}
}

func TestCreateProviderFromConfig_NearAITrimsConfiguredAPIKey(t *testing.T) {
	t.Setenv("NEARAI_API_KEY", "")

	provider, err := createProviderFromConfig("nearai", &config.ProviderConfig{
		ResolvedAPIKey: "  test-key\n",
		Model:          "zai-org/GLM-5.1-FP8",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nearai, ok := provider.(*NearAIProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *NearAIProvider", provider)
	}
	if nearai.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", nearai.apiKey, "test-key")
	}
}

func TestNearAIProviderCapabilities(t *testing.T) {
	provider := NewNearAIProvider("key", "")
	caps := provider.Capabilities()
	if caps.NativeWebSearch {
		t.Fatal("expected NativeWebSearch=false")
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

func TestNearAIProviderListModelsUsesCatalogEndpointAndFiltersNonChatModels(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/list" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want Bearer test-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": [
				{
					"modelId": "zai-org/GLM-5.1-FP8",
					"inputCostPerToken": {"amount": 850, "scale": 9, "currency": "USD"},
					"outputCostPerToken": {"amount": 3300, "scale": 9, "currency": "USD"},
					"metadata": {
						"contextLength": 202752,
						"modelDisplayName": "GLM 5.1",
						"ownedBy": "nearai",
						"architecture": {"inputModalities": ["text"], "outputModalities": ["text"]}
					}
				},
				{
					"modelId": "openai/gpt-oss-120b",
					"inputCostPerToken": {"amount": 150, "scale": 9, "currency": "USD"},
					"outputCostPerToken": {"amount": 550, "scale": 9, "currency": "USD"},
					"metadata": {"contextLength": 131000, "modelDisplayName": "GPT OSS 120B", "ownedBy": "nearai"}
				},
				{
					"modelId": "Qwen/Qwen3.6-35B-A3B-FP8",
					"metadata": {"contextLength": 262144, "architecture": {"inputModalities": ["text"], "outputModalities": ["text"]}}
				},
				{
					"modelId": "unknown/text-model",
					"metadata": {"architecture": {"inputModalities": ["text"], "outputModalities": ["text"]}}
				},
				{
					"modelId": "black-forest-labs/FLUX.2-klein-4B",
					"inputCostPerToken": {"amount": 1000, "scale": 9, "currency": "USD"},
					"outputCostPerToken": {"amount": 1000, "scale": 9, "currency": "USD"},
					"metadata": {"architecture": {"inputModalities": ["text"], "outputModalities": ["image"]}}
				},
				{
					"modelId": "Qwen/Qwen3-Embedding-0.6B",
					"metadata": {"architecture": {"inputModalities": ["text"], "outputModalities": ["embedding"]}}
				},
				{
					"modelId": "openai/whisper-large-v3",
					"metadata": {"architecture": {"inputModalities": ["audio"], "outputModalities": ["text"]}}
				},
				{"modelId": "Qwen/Qwen3-Reranker-0.6B"},
				{"modelId": "openai/privacy-filter"}
			]
		}`))
	}))
	defer ts.Close()

	provider := &NearAIProvider{OpenAICompatProvider: NewOpenAICompatProvider(ts.URL, "test-key", "zai-org/GLM-5.1-FP8", "NEAR AI")}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 4 {
		t.Fatalf("ListModels() returned %d models, want 4: %#v", len(models), models)
	}
	if models[0].ID != "zai-org/GLM-5.1-FP8" {
		t.Fatalf("first model = %q, want zai-org/GLM-5.1-FP8", models[0].ID)
	}
	if models[0].DisplayName != "GLM 5.1" || models[0].OwnedBy != "nearai" {
		t.Fatalf("first model metadata = %#v", models[0])
	}
	if models[0].InputLimit != 202_752 {
		t.Fatalf("GLM InputLimit = %d, want 202752", models[0].InputLimit)
	}
	if !nearPriceEqual(models[0].InputPrice, 0.85) || !nearPriceEqual(models[0].OutputPrice, 3.30) {
		t.Fatalf("GLM pricing = %g/%g, want 0.85/3.30", models[0].InputPrice, models[0].OutputPrice)
	}
	if models[1].ID != "openai/gpt-oss-120b" {
		t.Fatalf("second model = %q, want openai/gpt-oss-120b", models[1].ID)
	}
	if !nearPriceEqual(models[1].InputPrice, 0.15) || !nearPriceEqual(models[1].OutputPrice, 0.55) {
		t.Fatalf("gpt-oss pricing = %g/%g, want 0.15/0.55", models[1].InputPrice, models[1].OutputPrice)
	}
	if models[2].ID != "Qwen/Qwen3.6-35B-A3B-FP8" {
		t.Fatalf("third model = %q, want Qwen/Qwen3.6-35B-A3B-FP8", models[2].ID)
	}
	if !nearPriceEqual(models[2].InputPrice, 0.17) || !nearPriceEqual(models[2].OutputPrice, 1.10) {
		t.Fatalf("curated fallback pricing = %g/%g, want 0.17/1.10", models[2].InputPrice, models[2].OutputPrice)
	}
	if models[3].ID != "unknown/text-model" {
		t.Fatalf("fourth model = %q, want unknown/text-model", models[3].ID)
	}
	if models[3].InputPrice != -1 || models[3].OutputPrice != -1 {
		t.Fatalf("missing pricing = %g/%g, want -1/-1", models[3].InputPrice, models[3].OutputPrice)
	}
}

func nearPriceEqual(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}
