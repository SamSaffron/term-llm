package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestUsesEndpointModelCatalog(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.ProviderConfig
		want bool
	}{
		{"openai", config.ProviderConfig{}, false},
		{"openai", config.ProviderConfig{BaseURL: "https://api.openai.com/v1"}, false},
		{"openai", config.ProviderConfig{BaseURL: "https://api.openai.com/v1/"}, false},
		{"muse", config.ProviderConfig{Type: config.ProviderTypeOpenAI, BaseURL: "https://api.meta.ai/v1"}, true},
		{"my-server", config.ProviderConfig{Type: config.ProviderTypeOpenAICompat, BaseURL: "http://127.0.0.1:8080/v1"}, true},
		{"my-vllm", config.ProviderConfig{Type: config.ProviderTypeVLLM, BaseURL: "http://127.0.0.1:8000/v1"}, true},
		{"gateway", config.ProviderConfig{Type: config.ProviderTypeAnthropic, BaseURL: "https://gateway.example.com"}, false},
	} {
		if got := UsesEndpointModelCatalog(tc.name, tc.cfg); got != tc.want {
			t.Errorf("UsesEndpointModelCatalog(%q, %+v) = %v, want %v", tc.name, tc.cfg, got, tc.want)
		}
	}
}

// A custom-named provider of type openai pointed at another vendor's API
// must not be offered OpenAI's curated models, and its configured model
// must always complete.
func TestGetProviderCompletionsCustomOpenAIEndpointDoesNotBleedCuratedModels(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"muse": {
			Type:    config.ProviderTypeOpenAI,
			BaseURL: "https://api.meta.ai/v1",
			APIKey:  "muse-key",
			Model:   "muse-wonder-full-1.0-contributor",
		},
	}}

	want := []string{"muse:muse-wonder-full-1.0-contributor"}
	for _, prefix := range []string{"muse:", "muse:muse", "muse:muse-wonder-full-1.0-contributor"} {
		if got := GetProviderCompletions(prefix, false, cfg); !equalSlice(got, want) {
			t.Fatalf("GetProviderCompletions(%q) = %v, want %v", prefix, got, want)
		}
	}
	if got := GetProviderCompletions("muse:gpt", false, cfg); len(got) != 0 {
		t.Fatalf("curated OpenAI models bled into custom endpoint provider: %v", got)
	}
}

func TestEndpointModelCacheIsScopedToEndpointAndCredential(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	base := config.ProviderConfig{
		Type:    config.ProviderTypeOpenAI,
		BaseURL: "https://api.meta.ai/v1",
		Model:   "configured",
	}
	keyA := base
	keyA.APIKey = "key-a"
	keyB := base
	keyB.APIKey = "key-b"
	otherEndpoint := base
	otherEndpoint.APIKey = "key-a"
	otherEndpoint.BaseURL = "https://other.example.com/v1"

	RefreshEndpointCacheSync(keyA, []core.ModelInfo{{ID: "a-model"}})
	RefreshEndpointCacheSync(keyB, []core.ModelInfo{{ID: "b-model"}})
	RefreshEndpointCacheSync(otherEndpoint, []core.ModelInfo{{ID: "other-model"}})

	if got := GetCachedEndpointModels(keyA); !equalSlice(got, []string{"a-model"}) {
		t.Fatalf("cached models for key-a = %v, want [a-model]", got)
	}
	if got := GetCachedEndpointModels(keyB); !equalSlice(got, []string{"b-model"}) {
		t.Fatalf("cached models for key-b = %v, want [b-model]", got)
	}
	if got := GetCachedEndpointModels(otherEndpoint); !equalSlice(got, []string{"other-model"}) {
		t.Fatalf("cached models for other endpoint = %v, want [other-model]", got)
	}

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"muse": keyA}}
	want := []string{"muse:configured", "muse:a-model"}
	if got := GetProviderCompletions("muse:", false, cfg); !equalSlice(got, want) {
		t.Fatalf("completions = %v, want %v", got, want)
	}
}

func TestRefreshEndpointModelsIfStaleFetchesAndCaches(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/models" {
			t.Errorf("requested path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"muse-live-1"},{"id":"muse-live-2"}]}`)
	}))
	defer server.Close()

	providerCfg := config.ProviderConfig{
		Type:           config.ProviderTypeOpenAI,
		BaseURL:        server.URL + "/v1",
		ResolvedAPIKey: "test-key",
		Model:          "configured",
	}
	for range 2 {
		if err := RefreshEndpointModelsIfStale(context.Background(), providerCfg); err != nil {
			t.Fatalf("RefreshEndpointModelsIfStale: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("model requests = %d, want 1 (fresh cache reused)", got)
	}
	want := []string{"muse-live-1", "muse-live-2"}
	if got := GetCachedEndpointModels(providerCfg); !equalSlice(got, want) {
		t.Fatalf("cached models = %v, want %v", got, want)
	}
}

// The configured model stays available alongside the type's curated list
// for providers that do use their type's public catalog.
func TestGetProviderCompletionsIncludesConfiguredModelWithCuratedList(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"openai": {Model: "my-finetune"},
	}}
	got := GetProviderCompletions("openai:my-", false, cfg)
	if !equalSlice(got, []string{"openai:my-finetune"}) {
		t.Fatalf("completions = %v, want [openai:my-finetune]", got)
	}
}
