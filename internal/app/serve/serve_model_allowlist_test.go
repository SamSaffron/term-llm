package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
)

func allowedDiscoveryServer(t *testing.T) *serveServer {
	t.Helper()
	clearProviderCredentialEnv(t)
	cfg := &config.Config{
		DefaultProvider:   "corp",
		ProviderDiscovery: config.ProviderDiscoveryConfig,
		Providers: map[string]config.ProviderConfig{"corp": {
			Type:          config.ProviderTypeOpenAICompat,
			Model:         "friendly",
			Models:        []string{"friendly", "blocked", "pinned"},
			AllowedModels: []string{"upstream/model-high"},
			ModelConfigs: []config.ProviderModelConfig{{
				ID: "upstream/model", Alias: "friendly", ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "low",
			}},
		}},
	}
	return &serveServer{
		cfgRef:          cfg,
		modelsProviders: map[string]core.Provider{"corp": mockprovider.NewMockProvider("corp")},
	}
}

func TestHandleModelsFiltersProviderAllowedModels(t *testing.T) {
	srv := allowedDiscoveryServer(t)
	rr := httptest.NewRecorder()
	srv.handleModels(rr, httptest.NewRequest(http.MethodGet, "/v1/models?provider=corp", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []struct {
			ID                   string   `json:"id"`
			ReasoningEfforts     []string `json:"reasoning_efforts"`
			DefaultReasoningMode string   `json:"default_reasoning_effort"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(result.Data) != 1 || result.Data[0].ID != "upstream/model-high" {
		t.Fatalf("models = %#v, want the pinned allowed effort only", result.Data)
	}
	if !slices.Equal(result.Data[0].ReasoningEfforts, []string{"high"}) {
		t.Fatalf("reasoning_efforts = %v, want [high]", result.Data[0].ReasoningEfforts)
	}
	if result.Data[0].DefaultReasoningMode != "" {
		t.Fatalf("default_reasoning_effort = %q, want blocked default omitted", result.Data[0].DefaultReasoningMode)
	}
}

func TestHandleModelsListsAllowedModelsWhenConfiguredDefaultIsBlocked(t *testing.T) {
	clearProviderCredentialEnv(t)
	srv := &serveServer{cfgRef: &config.Config{
		DefaultProvider:   "debug",
		ProviderDiscovery: config.ProviderDiscoveryConfig,
		Providers: map[string]config.ProviderConfig{"debug": {
			Model: "blocked", Models: []string{"blocked"}, AllowedModels: []string{"allowed"},
		}},
	}}
	rr := httptest.NewRecorder()
	srv.handleModels(rr, httptest.NewRequest(http.MethodGet, "/v1/models?provider=debug", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(result.Data) != 1 || result.Data[0].ID != "allowed" {
		t.Fatalf("models = %#v, want the allowed model despite a blocked default", result.Data)
	}
}

func TestHandleProvidersFiltersProviderAllowedModels(t *testing.T) {
	srv := allowedDiscoveryServer(t)
	rr := httptest.NewRecorder()
	srv.handleProviders(rr, httptest.NewRequest(http.MethodGet, "/v1/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []struct {
			Name         string   `json:"name"`
			Models       []string `json:"models"`
			DefaultModel string   `json:"default_model"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	for _, provider := range result.Data {
		if provider.Name != "corp" {
			continue
		}
		if want := []string{"upstream/model-high"}; !slices.Equal(provider.Models, want) {
			t.Fatalf("corp models = %v, want %v", provider.Models, want)
		}
		if provider.DefaultModel != "" {
			t.Fatalf("blocked default_model advertised: %q", provider.DefaultModel)
		}
		return
	}
	t.Fatalf("corp missing from /v1/providers: %s", rr.Body.String())
}

func TestHandleModelsDoesNotRestoreFilteredDefaultEffort(t *testing.T) {
	srv := allowedDiscoveryServer(t)
	srv.setCachedModelsForProvider("corp", []core.ModelInfo{{ID: "upstream/model-high", ReasoningEfforts: []string{"low"}, DefaultReasoningEffort: "low"}})
	rr := httptest.NewRecorder()
	srv.handleModels(rr, httptest.NewRequest(http.MethodGet, "/v1/models?provider=corp", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []struct {
			ID      string   `json:"id"`
			Efforts []string `json:"reasoning_efforts"`
			Default string   `json:"default_reasoning_effort"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != 1 || result.Data[0].Default != "" {
		t.Fatalf("blocked default reasoning effort survived filtering: %+v", result.Data)
	}
}
