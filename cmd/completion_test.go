package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
)

func TestRefreshGrokBinCompletionCache(t *testing.T) {
	original := refreshGrokBinModelsForCompletion
	t.Cleanup(func() { refreshGrokBinModelsForCompletion = original })

	var calls int
	var gotModel string
	var gotEnv map[string]string
	refreshGrokBinModelsForCompletion = func(ctx context.Context, model string, env map[string]string) error {
		calls++
		gotModel = model
		gotEnv = env
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("completion refresh context has no deadline")
		}
		return nil
	}

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"my-grok": {
			Type:  config.ProviderTypeGrokBin,
			Model: "grok-4.6",
			Env:   map[string]string{"GROK_AUTH_PATH": "/tmp/auth.json"},
		},
		"pinned-grok": {
			Type:   config.ProviderTypeGrokBin,
			Models: []string{"grok-custom"},
		},
	}}

	refreshGrokBinCompletionCache("my-grok:gro", cfg)
	if calls != 1 || gotModel != "grok-4.6" || gotEnv["GROK_AUTH_PATH"] != "/tmp/auth.json" {
		t.Fatalf("refresh calls/model/env = %d/%q/%v", calls, gotModel, gotEnv)
	}

	refreshGrokBinCompletionCache("grok-bin:", nil)
	if calls != 2 {
		t.Fatalf("built-in grok-bin refresh calls = %d, want 2", calls)
	}

	refreshGrokBinCompletionCache("my-grok", cfg)
	refreshGrokBinCompletionCache("pinned-grok:", cfg)
	refreshGrokBinCompletionCache("openai:gpt", cfg)
	if calls != 2 {
		t.Fatalf("inapplicable completion unexpectedly refreshed: calls = %d", calls)
	}
}

func TestRefreshZenCompletionCache(t *testing.T) {
	original := refreshZenModelsForCompletion
	t.Cleanup(func() { refreshZenModelsForCompletion = original })

	var calls int
	var gotAPIKey, gotModel string
	refreshZenModelsForCompletion = func(ctx context.Context, apiKey, model string) error {
		calls++
		gotAPIKey = apiKey
		gotModel = model
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("completion refresh context has no deadline")
		}
		return nil
	}

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"work-zen": {
			Type:   config.ProviderTypeZen,
			APIKey: "paid-key",
			Model:  "paid-model",
		},
		"pinned-zen": {
			Type:   config.ProviderTypeZen,
			Models: []string{"fixed-model"},
		},
	}}

	refreshZenCompletionCache("work-zen:paid", cfg)
	if calls != 1 || gotAPIKey != "paid-key" || gotModel != "paid-model" {
		t.Fatalf("refresh calls/key/model = %d/%q/%q", calls, gotAPIKey, gotModel)
	}

	refreshZenCompletionCache("zen:", nil)
	if calls != 2 {
		t.Fatalf("built-in Zen refresh calls = %d, want 2", calls)
	}

	refreshZenCompletionCache("work-zen", cfg)
	refreshZenCompletionCache("pinned-zen:", cfg)
	refreshZenCompletionCache("openai:gpt", cfg)
	if calls != 2 {
		t.Fatalf("inapplicable completion unexpectedly refreshed: calls = %d", calls)
	}
}

func TestOllamaModelCompletionsUseLiveEndpoint(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"remote-only:latest"},{"name":"remote-vision:latest"}]}`)
		case "/api/show":
			var req struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode show request: %v", err)
				return
			}
			if req.Model == "remote-only:latest" {
				fmt.Fprint(w, `{"capabilities":["completion","thinking"]}`)
			} else {
				fmt.Fprint(w, `{"capabilities":["completion"]}`)
			}
		default:
			t.Errorf("requested unexpected path %q", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"remote": {
			Type:    config.ProviderTypeOllama,
			BaseURL: server.URL,
			Model:   "remote-only:latest",
		},
	}}

	refreshOllamaCompletionCache("remote:remote-", cfg)
	got := llm.GetProviderCompletions("remote:remote-", false, cfg)
	want := []string{
		"remote:remote-only:latest",
		"remote:remote-only:latest-low",
		"remote:remote-only:latest-medium",
		"remote:remote-only:latest-high",
		"remote:remote-only:latest-xhigh",
		"remote:remote-vision:latest",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("provider completions = %v, want %v", got, want)
	}
	if slices.Contains(got, "remote:qwen2.5-coder:7b") {
		t.Fatalf("provider completions unexpectedly contain static fallback: %v", got)
	}

	wantModels := []string{
		"remote-only:latest",
		"remote-only:latest-low",
		"remote-only:latest-medium",
		"remote-only:latest-high",
		"remote-only:latest-xhigh",
		"remote-vision:latest",
	}
	if gotModels := ollamaModelCompletions("remote", cfg); !slices.Equal(gotModels, wantModels) {
		t.Fatalf("config model completions = %v, want %v", gotModels, wantModels)
	}
	wantProviderEfforts := []string{"remote-high", "remote-low", "remote-medium", "remote-xhigh"}
	if gotProviders := llm.GetProviderCompletions("remote-", false, cfg); !slices.Equal(gotProviders, wantProviderEfforts) {
		t.Fatalf("provider effort completions = %v, want %v", gotProviders, wantProviderEfforts)
	}
}

func TestRefreshOllamaCompletionCacheUsesLongestProviderPrefix(t *testing.T) {
	original := refreshOllamaModelsForCompletion
	t.Cleanup(func() { refreshOllamaModelsForCompletion = original })

	var gotBaseURL string
	refreshOllamaModelsForCompletion = func(_ context.Context, baseURL, _ string) error {
		gotBaseURL = baseURL
		return nil
	}
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama":        {Type: config.ProviderTypeOllama, BaseURL: "http://short.example"},
		"ollama-remote": {Type: config.ProviderTypeOllama, BaseURL: "http://long.example"},
	}}
	refreshOllamaCompletionCache("ollama-remote-high", cfg)
	if gotBaseURL != "http://long.example" {
		t.Fatalf("refreshed base URL = %q, want longest matching provider", gotBaseURL)
	}
}

func TestProviderFlagCompletionDirectiveFinishesConfiguredEffortProfile(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"cdck_deepseek": {
			Model: "deepseek-ai/DeepSeek-V4-Flash",
			ModelConfigs: []config.ProviderModelConfig{{
				ID:               "deepseek-ai/DeepSeek-V4-Flash",
				ReasoningEfforts: []string{"none", "low", "high", "max"},
			}},
		},
		"claude": {
			Model: "vendor/model",
			ModelConfigs: []config.ProviderModelConfig{{
				ID:               "vendor/model",
				ReasoningEfforts: []string{"bin", "high"},
			}},
		},
	}}

	if got := providerFlagCompletionDirective(cfg, "cdck_deepseek-"); got != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("effort profile directive = %v, want NoFileComp", got)
	}
	wantBare := cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	if got := providerFlagCompletionDirective(cfg, "cdck_deepseek"); got != wantBare {
		t.Fatalf("bare provider directive = %v, want %v", got, wantBare)
	}
	if got := providerFlagCompletionDirective(cfg, "claude-bin"); got != wantBare {
		t.Fatalf("built-in provider collision directive = %v, want %v", got, wantBare)
	}
	if got := providerFlagCompletionDirective(cfg, "cdck_deepseek-unexpected"); got != wantBare {
		t.Fatalf("invalid effort prefix directive = %v, want %v", got, wantBare)
	}
}
