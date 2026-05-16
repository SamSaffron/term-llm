package llm

import (
	"context"
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
