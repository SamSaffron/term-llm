package responsesproviders

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
	if got := provider.Model(); got != "big-pickle" {
		t.Fatalf("NewZenProvider model = %q, want big-pickle", got)
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
		_, _ = w.Write([]byte(`{"data":[{"id":"basic-model"},{"id":"mimo-v2.5-free"},{"id":"big-pickle"}]}`))
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
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-v4-flash"}]}`))
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
	if len(models) != 1 || models[0].ID != "deepseek-v4-flash" {
		t.Fatalf("models = %+v, want [deepseek-v4-flash]", models)
	}
}

func TestCachedZenModelsExcludeFreeRoutes(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	RefreshZenCacheSync([]ModelInfo{{ID: "big-pickle"}, {ID: "mimo-v2.5-free"}, {ID: "deepseek-v4-flash"}})
	got := GetCachedZenModels()
	if len(got) != 1 || got[0] != "deepseek-v4-flash" {
		t.Fatalf("cached Zen models = %v", got)
	}
}
