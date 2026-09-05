package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeZenModel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"bigpickle", "big-pickle"},
		{"BigPickle", "big-pickle"},
		{"big_pickle", "big-pickle"},
		{"big pickle", "big-pickle"},
		{" big-pickle ", "big-pickle"},
		{"mimo-v2.5-free", "mimo-v2.5-free"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeZenModel(tt.in); got != tt.want {
				t.Fatalf("normalizeZenModel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewZenProviderNormalizesBigPickleAlias(t *testing.T) {
	provider := NewZenProvider("", "bigpickle")
	if got := provider.model; got != "big-pickle" {
		t.Fatalf("NewZenProvider model = %q, want big-pickle", got)
	}
}

func TestZenListModelsIncludesReasoningEfforts(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-nano"},{"id":"qwen3.6-plus-free"}]}`))
	}))
	defer modelsServer.Close()

	pricingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"opencode":{"models":{"gpt-5-nano":{"id":"gpt-5-nano","name":"GPT-5 Nano","cost":{"input":0,"output":0}}}}}`))
	}))
	defer pricingServer.Close()

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"opencode":{"npm":"@ai-sdk/openai-compatible","api":"https://opencode.ai/zen/v1","models":{
			"gpt-5-nano":{"id":"gpt-5-nano","reasoning_options":[{"type":"effort","values":["minimal","low","medium","high"]}]},
			"qwen3.6-plus-free":{"id":"qwen3.6-plus-free","reasoning_options":[{"type":"toggle"},{"type":"budget_tokens","max":81920}]},
			"x-preview-f-free":{"id":"x-preview-f-free","reasoning_options":[{"type":"effort","values":["low","high","max"]}]}
		}}}`))
	}))
	defer catalogServer.Close()

	restoreModelsDevURL := modelsDevURL
	restoreCatalogURL := zenCatalogURL
	modelsDevURL = pricingServer.URL
	zenCatalogURL = catalogServer.URL
	defer func() {
		modelsDevURL = restoreModelsDevURL
		zenCatalogURL = restoreCatalogURL
	}()

	provider := &ZenProvider{OpenAICompatProvider: NewOpenAICompatProvider(modelsServer.URL, "", "", zenDisplayName)}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
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

	qwen, ok := byID["qwen3.6-plus-free"]
	if !ok {
		t.Fatalf("qwen3.6-plus-free missing from results: %+v", models)
	}
	// Toggle/budget reasoning options do not accept plain effort strings.
	if len(qwen.ReasoningEfforts) != 0 {
		t.Fatalf("qwen3.6-plus-free efforts = %v, want none", qwen.ReasoningEfforts)
	}

	got := GetProviderCompletions("zen:", false, nil)
	wantCompletions := []string{
		"zen:gpt-5-nano",
		"zen:gpt-5-nano-minimal",
		"zen:gpt-5-nano-low",
		"zen:gpt-5-nano-medium",
		"zen:gpt-5-nano-high",
		"zen:qwen3.6-plus-free",
	}
	if !equalSlice(got, wantCompletions) {
		t.Fatalf("Zen completions = %v, want live catalog %v", got, wantCompletions)
	}
	if containsModelID(got, "zen:mimo-v2.5-free") {
		t.Fatalf("Zen completions unexpectedly contain curated fallback: %v", got)
	}
}

func TestZenListModelsPreservesEnrichedCacheWhenPricingFails(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	RefreshZenCacheSync([]ModelInfo{{
		ID:               "cached-model",
		DisplayName:      "Cached Model",
		InputPrice:       1.25,
		OutputPrice:      2.5,
		ReasoningEfforts: []string{"low", "high"},
	}})

	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"basic-model"}]}`))
	}))
	defer modelsServer.Close()
	pricingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer pricingServer.Close()
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer catalogServer.Close()

	restoreModelsDevURL := modelsDevURL
	restoreCatalogURL := zenCatalogURL
	modelsDevURL = pricingServer.URL
	zenCatalogURL = catalogServer.URL
	defer func() {
		modelsDevURL = restoreModelsDevURL
		zenCatalogURL = restoreCatalogURL
	}()

	provider := &ZenProvider{OpenAICompatProvider: NewOpenAICompatProvider(modelsServer.URL, "", "", zenDisplayName)}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "basic-model" {
		t.Fatalf("fallback models = %+v, want [basic-model]", models)
	}

	cached := GetCachedZenModelInfos()
	if len(cached) != 1 || cached[0].ID != "cached-model" || cached[0].DisplayName != "Cached Model" || cached[0].InputPrice != 1.25 || cached[0].OutputPrice != 2.5 || !equalSlice(cached[0].ReasoningEfforts, []string{"low", "high"}) {
		t.Fatalf("cached metadata was replaced by degraded listing: %+v", cached)
	}
}

func TestZenListModelsSurvivesCatalogFailure(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"big-pickle"}]}`))
	}))
	defer modelsServer.Close()

	pricingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"opencode":{"models":{}}}`))
	}))
	defer pricingServer.Close()

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer catalogServer.Close()

	restoreModelsDevURL := modelsDevURL
	restoreCatalogURL := zenCatalogURL
	modelsDevURL = pricingServer.URL
	zenCatalogURL = catalogServer.URL
	defer func() {
		modelsDevURL = restoreModelsDevURL
		zenCatalogURL = restoreCatalogURL
	}()

	provider := &ZenProvider{OpenAICompatProvider: NewOpenAICompatProvider(modelsServer.URL, "", "", zenDisplayName)}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "big-pickle" {
		t.Fatalf("models = %+v, want [big-pickle]", models)
	}
}
