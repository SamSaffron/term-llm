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
		if got := req.Header.Get("originator"); got != chatGPTCodexOriginator {
			t.Fatalf("originator = %q, want %q", got, chatGPTCodexOriginator)
		}
		if got := req.Header.Get("User-Agent"); got != chatGPTCodexUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, chatGPTCodexUserAgent)
		}
		if got := req.Header.Get("version"); got != chatGPTCodexClientVersion {
			t.Fatalf("version = %q, want %q", got, chatGPTCodexClientVersion)
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
		FetchedAt:     time.Now().Add(-time.Hour),
		ClientVersion: chatGPTModelsClientVersion,
		Models:        []ModelInfo{{ID: "gpt-5.5", ServiceTiers: []ModelServiceTier{{ID: ServiceTierFast}}}},
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
	wantEfforts := []string{"low", "medium", "ultra"}
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
		SupportedReasoningLevels: chatGPTReasoningLevels{"low", "medium", "max"},
		DefaultReasoningLevel:    "low",
		DefaultReasoningEffort:   "medium",
	}).toModelInfo()
	if got.ID != "gpt-5.6-luna" || got.DisplayName != "Luna title" || got.InputLimit != 400 {
		t.Fatalf("toModelInfo() = %#v", got)
	}
	if got.DefaultReasoningEffort != "medium" {
		t.Fatalf("DefaultReasoningEffort = %q, want explicit medium", got.DefaultReasoningEffort)
	}
}
