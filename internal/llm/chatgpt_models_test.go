package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestChatGPTListModelsFetchesAndCachesServiceTiers(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	calls := 0
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/backend-api/codex/models" {
			t.Fatalf("path = %q", req.URL.Path)
		}
		if got := req.URL.Query().Get("client_version"); got != chatGPTModelsClientVersion {
			t.Fatalf("client_version = %q, want %q", got, chatGPTModelsClientVersion)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("originator"); got != chatGPTOriginator {
			t.Fatalf("originator = %q, want %q", got, chatGPTOriginator)
		}
		if got := req.Header.Get("User-Agent"); got != chatGPTUserAgent() {
			t.Fatalf("User-Agent = %q, want %q", got, chatGPTUserAgent())
		}
		if got := req.Header.Get("version"); got != "" {
			t.Fatalf("version = %q, want omitted (not a Codex application)", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"ETag": []string{"etag-1"}},
			Body: io.NopCloser(strings.NewReader(`{
				"models": [{
					"slug": "gpt-5.5",
					"display_name": "GPT 5.5",
					"max_input_tokens": 123,
					"service_tiers": [{"id":"priority","name":"fast","description":"Priority processing"}],
					"additional_speed_tiers": ["fast"]
				}]
			}`)),
		}, nil
	})}

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, "gpt-5.5-medium")

	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.5" || models[0].InputLimit != 123 {
		t.Fatalf("unexpected models: %#v", models)
	}
	if !ModelSupportsFast(models[0]) {
		t.Fatalf("expected fast support: %#v", models[0])
	}

	models, err = provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("cached ListModels: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls after cache = %d, want 1", calls)
	}
}

func TestChatGPTListModelsFallsBackToStaleCache(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	if err := saveChatGPTModelsCache(chatGPTModelsCache{
		AccountID:     "test-account",
		FetchedAt:     time.Now().Add(-time.Hour),
		ClientVersion: chatGPTModelsClientVersion,
		Models: []ModelInfo{{
			ID:                     "gpt-5.5",
			ServiceTiers:           []ModelServiceTier{{ID: ServiceTierFast}},
			ReasoningEfforts:       []string{"medium", "max", "ultra"},
			DefaultReasoningEffort: "ultra",
		}},
	}); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, "gpt-5.5-medium")

	models, fresh, err := provider.ListModelsWithFreshness(context.Background())
	if err != nil {
		t.Fatalf("ListModels stale fallback: %v", err)
	}
	if fresh {
		t.Fatal("expected stale fallback to report fresh=false")
	}
	if len(models) != 1 || models[0].ID != "gpt-5.5" {
		t.Fatalf("unexpected stale models: %#v", models)
	}
	if want := []string{"medium", "max"}; !equalSlice(models[0].ReasoningEfforts, want) {
		t.Fatalf("stale ReasoningEfforts = %v, want %v", models[0].ReasoningEfforts, want)
	}
	if models[0].DefaultReasoningEffort != "max" {
		t.Fatalf("stale DefaultReasoningEffort = %q, want max", models[0].DefaultReasoningEffort)
	}
}

func TestChatGPTModelInfoDecodesReasoningMetadata(t *testing.T) {
	var response chatGPTModelsResponse
	if err := json.Unmarshal([]byte(`{
		"models": [
			{
				"slug": "gpt-5.6-sol",
				"display_name": "GPT-5.6 Sol",
				"input_token_limit": 372000,
				"supported_reasoning_levels": [
					{"effort": "low", "description": "Fast"},
					{"level": "medium"},
					"ultra"
				],
				"default_reasoning_level": "medium"
			}
		]
	}`), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(response.Models) != 1 {
		t.Fatalf("models = %#v", response.Models)
	}
	got := response.Models[0].toModelInfo()
	if got.ID != "gpt-5.6-sol" || got.DisplayName != "GPT-5.6 Sol" || got.InputLimit != 372_000 {
		t.Fatalf("model identity/limit = %#v", got)
	}
	wantEfforts := []string{"low", "medium", "max"}
	if !equalSlice(got.ReasoningEfforts, wantEfforts) {
		t.Fatalf("ReasoningEfforts = %v, want %v", got.ReasoningEfforts, wantEfforts)
	}
	if got.DefaultReasoningEffort != "medium" {
		t.Fatalf("DefaultReasoningEffort = %q, want medium", got.DefaultReasoningEffort)
	}
}

func TestChatGPTModelInfoPrefersExplicitMetadata(t *testing.T) {
	got := (chatGPTModelInfo{
		Slug:                     "gpt-5.6-luna",
		ID:                       "ignored-id",
		Title:                    "Luna title",
		Name:                     "Luna name",
		MaxInputTokens:           400,
		InputTokenLimit:          300,
		ContextWindow:            200,
		MaxContextWindow:         1000,
		SupportedReasoningLevels: chatGPTReasoningLevels{"low", "medium", "max"},
		DefaultReasoningLevel:    "low",
		DefaultReasoningEffort:   "medium",
	}).toModelInfo()
	if got.ID != "gpt-5.6-luna" || got.DisplayName != "Luna title" || got.InputLimit != 400 {
		t.Fatalf("toModelInfo() = %#v", got)
	}
	if got.OutputLimit != 128_000 {
		t.Fatalf("OutputLimit = %d, want static fallback 128000", got.OutputLimit)
	}
	if got.DefaultReasoningEffort != "medium" {
		t.Fatalf("DefaultReasoningEffort = %q, want explicit medium", got.DefaultReasoningEffort)
	}
}

func TestChatGPTModelInfoPrefersActiveContextOverMaximum(t *testing.T) {
	got := (chatGPTModelInfo{
		Slug:             "account-preview",
		ContextWindow:    272_000,
		MaxContextWindow: 872_000,
	}).toModelInfo()
	if got.InputLimit != 272_000 {
		t.Fatalf("InputLimit = %d, want active context_window 272000", got.InputLimit)
	}
}

func TestChatGPTListModelsRetainsHiddenModelsForExplicitSelection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{"models":[
				{"slug":"visible","visibility":"list","context_window":272000},
				{"slug":"hidden","visibility":"hide","context_window":872000},
				{"slug":"legacy","context_window":100000}
			]}`)),
		}, nil
	})}
	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, "visible")

	models, fresh, err := provider.ListModelsWithFreshness(context.Background())
	if err != nil || !fresh {
		t.Fatalf("ListModelsWithFreshness() = fresh %v, error %v", fresh, err)
	}
	if len(models) != 3 || models[0].ID != "visible" || models[1].ID != "hidden" || models[2].ID != "legacy" {
		t.Fatalf("models = %#v, want complete remote catalog", models)
	}
}

func TestChatGPTListModelsFallsBackToStaticCatalogOnColdFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, "gpt-5.6-sol")

	models, fresh, err := provider.ListModelsWithFreshness(context.Background())
	if err != nil || fresh {
		t.Fatalf("ListModelsWithFreshness() = fresh %v, error %v", fresh, err)
	}
	if len(models) != len(ProviderModels["chatgpt"]) || models[0].ID != "gpt-6-astra" {
		t.Fatalf("static fallback = %#v", models)
	}
}

func TestChatGPTCacheIsAccountScoped(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := saveChatGPTModelsCache(chatGPTModelsCache{
		AccountID:     "account-a",
		FetchedAt:     time.Now(),
		ClientVersion: chatGPTModelsClientVersion,
		Models:        []ModelInfo{{ID: "private-preview"}},
	}); err != nil {
		t.Fatalf("save cache: %v", err)
	}
	if _, err := loadChatGPTModelsCacheForAccount(chatGPTModelsClientVersion, "account-a"); err != nil {
		t.Fatalf("load matching account: %v", err)
	}
	if _, err := loadChatGPTModelsCacheForAccount(chatGPTModelsClientVersion, "account-b"); err == nil {
		t.Fatal("expected cache from another account to be rejected")
	}
}

func TestChatGPTCachedCatalogDrivesModelsAndContextLimit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := saveChatGPTModelsCache(chatGPTModelsCache{
		FetchedAt:     time.Now(),
		ClientVersion: chatGPTModelsClientVersion,
		Models:        []ModelInfo{{ID: "account-preview", InputLimit: 333_000}},
	}); err != nil {
		t.Fatalf("save cache: %v", err)
	}
	ids := ProviderModelIDs("chatgpt")
	if len(ids) != 1 || ids[0] != "account-preview" {
		t.Fatalf("ProviderModelIDs(chatgpt) = %v", ids)
	}
	if got := InputLimitForProviderModel("chatgpt", "account-preview-high"); got != 333_000 {
		t.Fatalf("dynamic input limit = %d, want 333000", got)
	}
}
