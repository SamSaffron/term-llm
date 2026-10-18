package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
)

func TestShellCompletionSkipsInheritedPprofServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("TERM_LLM_PPROF", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))

	oldOut := rootCmd.OutOrStdout()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	defer func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(oldOut)
	}()

	if err := executeWithArgs([]string{cobra.ShellCompRequestCmd, "chat", ""}); err != nil {
		t.Fatalf("shell completion with occupied inherited pprof port: %v", err)
	}
	if output := strings.TrimSpace(out.String()); !strings.HasSuffix(output, ":0") {
		t.Fatalf("shell completion produced no trailing default directive: %q", out.String())
	}
}

func TestRefreshAgyBinCompletionCache(t *testing.T) {
	original := refreshAgyBinModelsForCompletion
	t.Cleanup(func() { refreshAgyBinModelsForCompletion = original })

	var calls int
	var gotModel string
	var gotEnv map[string]string
	refreshAgyBinModelsForCompletion = func(ctx context.Context, model string, env map[string]string) error {
		calls++
		gotModel = model
		gotEnv = env
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("completion refresh context has no deadline")
		}
		return nil
	}

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"my-agy": {
			Type:  config.ProviderTypeAgyBin,
			Model: "gemini-3.8-flash-high",
			Env:   map[string]string{"AGY_TEST": "value"},
		},
		"pinned-agy": {
			Type:   config.ProviderTypeAgyBin,
			Models: []string{"gemini-custom"},
		},
	}}

	refreshAgyBinCompletionCache("my-agy:gem", cfg)
	if calls != 1 || gotModel != "gemini-3.8-flash-high" || gotEnv["AGY_TEST"] != "value" {
		t.Fatalf("refresh calls/model/env = %d/%q/%v", calls, gotModel, gotEnv)
	}

	refreshAgyBinCompletionCache("agy-bin:", nil)
	if calls != 2 {
		t.Fatalf("built-in agy-bin refresh calls = %d, want 2", calls)
	}

	refreshAgyBinCompletionCache("my-agy", cfg)
	refreshAgyBinCompletionCache("pinned-agy:", cfg)
	refreshAgyBinCompletionCache("openai:gpt", cfg)
	if calls != 2 {
		t.Fatalf("inapplicable completion unexpectedly refreshed: calls = %d", calls)
	}
}

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

	// provider_discovery: config never probes a provider it does not enable.
	cfg.ProviderDiscovery = config.ProviderDiscoveryConfig
	refreshProviderCompletionCaches("grok-bin:", cfg)
	if calls != 2 {
		t.Fatalf("refreshed grok-bin, which provider_discovery does not enable: calls = %d", calls)
	}
	refreshProviderCompletionCaches("my-grok:gro", cfg)
	if calls != 3 {
		t.Fatalf("declared my-grok refresh calls = %d, want 3", calls)
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

func TestRefreshEndpointCompletionCache(t *testing.T) {
	original := refreshEndpointModelsForCompletion
	t.Cleanup(func() { refreshEndpointModelsForCompletion = original })

	var calls int
	var got config.ProviderConfig
	refreshEndpointModelsForCompletion = func(ctx context.Context, providerCfg config.ProviderConfig) error {
		calls++
		got = providerCfg
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("completion refresh context has no deadline")
		}
		return nil
	}

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"muse": {
			Type:    config.ProviderTypeOpenAI,
			BaseURL: "https://api.meta.ai/v1",
			APIKey:  "muse-key",
			Model:   "muse-wonder-full-1.0-contributor",
		},
		"pinned-muse": {
			Type:    config.ProviderTypeOpenAI,
			BaseURL: "https://api.meta.ai/v1",
			Models:  []string{"fixed-model"},
		},
		"openai": {Model: "gpt-5.4"},
	}}

	refreshEndpointCompletionCache("muse:muse-", cfg)
	if calls != 1 || got.BaseURL != "https://api.meta.ai/v1" || got.ResolvedAPIKey != "muse-key" || got.Model != "muse-wonder-full-1.0-contributor" {
		t.Fatalf("refresh calls/cfg = %d/%+v", calls, got)
	}

	// Provider-name stage, pinned model lists, and the public OpenAI
	// catalog never trigger an endpoint fetch.
	for _, toComplete := range []string{"muse", "pinned-muse:", "openai:gpt", "muse-other:"} {
		refreshEndpointCompletionCache(toComplete, cfg)
	}
	if calls != 1 {
		t.Fatalf("unexpected endpoint refreshes: %d", calls)
	}
}

func TestEndpointCompletionUsesLiveEndpointModels(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("requested path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"muse-live-1"},{"id":"muse-live-2"}]}`)
	}))
	defer server.Close()

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"muse": {
			Type:    config.ProviderTypeOpenAI,
			BaseURL: server.URL + "/v1",
			APIKey:  "test-key",
			Model:   "muse-configured",
		},
	}}

	refreshEndpointCompletionCache("muse:", cfg)
	got := llm.GetProviderCompletions("muse:", false, cfg)
	want := []string{"muse:muse-configured", "muse:muse-live-1", "muse:muse-live-2"}
	if !slices.Equal(got, want) {
		t.Fatalf("provider completions = %v, want %v", got, want)
	}
	if got := llm.GetProviderCompletions("muse:gpt", false, cfg); len(got) != 0 {
		t.Fatalf("curated OpenAI models bled into custom endpoint provider: %v", got)
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

func TestUsageProviderFlagCompletionIncludesGrok(t *testing.T) {
	completions, directive := UsageProviderFlagCompletion(nil, nil, "gr")
	if !slices.Contains(completions, "grok") {
		t.Fatalf("usage provider completions = %v, want grok", completions)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v, want NoFileComp", directive)
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

func TestProviderFlagCompletionProgressiveSpacing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, tc := range []struct {
		prefix  string
		noSpace bool
	}{
		{"claude-bin:", true},
		{"claude-bin:op", true},
		{"claude-bin:opus", true},
		{"claude-bin:opus-l", false},
		{"claude-bin:opus-low", false},
		{"claude-bin:haiku", false},
		{"openai:gpt-5.4", true},
		{"openai:gpt-5.4-low", false},
	} {
		_, directive := ProviderFlagCompletion(nil, nil, tc.prefix)
		if got := directive&cobra.ShellCompDirectiveNoSpace != 0; got != tc.noSpace {
			t.Errorf("%s: no-space = %v, want %v", tc.prefix, got, tc.noSpace)
		}
	}
}

func TestImageCompletionLoadsNamedProviderConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	text := "default_provider: debug\nimage:\n  provider: janus\nproviders:\n  janus:\n    type: openai_compatible\n    base_url: http://127.0.0.1:8080/v1\n    model: Janus-Pro-1B\n"
	if err = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	got, _ := ImageProviderFlagCompletion(nil, nil, "janus")
	if !slices.Contains(got, "janus") {
		t.Fatalf("provider completion = %v", got)
	}
	got, _ = ImageProviderFlagCompletion(nil, nil, "janus:")
	if !slices.Contains(got, "janus:Janus-Pro-1B") {
		t.Fatalf("model completion = %v", got)
	}
	if got := configValueCompletions("image.provider", "janus"); !slices.Contains(got, "janus") {
		t.Fatalf("config completion = %v", got)
	}
}

func TestRefreshAnthropicCompletionCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	original := refreshAnthropicModelsForCompletion
	t.Cleanup(func() { refreshAnthropicModelsForCompletion = original })

	var calls int
	var got config.ProviderConfig
	refreshAnthropicModelsForCompletion = func(ctx context.Context, providerCfg config.ProviderConfig) error {
		calls++
		got = providerCfg
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("completion refresh context has no deadline")
		}
		return nil
	}

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"anthropic":     {APIKey: "sk-test", Headers: map[string]string{"anthropic-workspace-id": "wrkspc_x"}},
		"gateway":       {Type: config.ProviderTypeAnthropic, BaseURL: "https://gateway.example.com"},
		"pinned-claude": {Type: config.ProviderTypeAnthropic, Models: []string{"fixed"}},
	}}

	refreshAnthropicCompletionCache("anthropic:claude-", cfg)
	if calls != 1 || got.ResolvedAPIKey != "sk-test" || got.Headers["anthropic-workspace-id"] != "wrkspc_x" {
		t.Fatalf("refresh calls=%d key=%q headers=%v", calls, got.ResolvedAPIKey, got.Headers)
	}

	// A refresh that did not populate the cache is not retried immediately.
	refreshAnthropicCompletionCache("anthropic:claude-o", cfg)
	if calls != 1 {
		t.Fatalf("refresh retried within the retry interval: %d calls", calls)
	}

	// Provider-name stage, custom base URLs, pinned model lists, and other
	// providers never trigger a catalog fetch.
	for _, toComplete := range []string{"anthropic", "gateway:claude", "pinned-claude:", "openai:gpt"} {
		refreshAnthropicCompletionCache(toComplete, cfg)
	}
	if calls != 1 {
		t.Fatalf("unexpected refreshes: %d", calls)
	}

	// An ANTHROPIC_BASE_URL gateway is never refreshed (its catalog is not
	// cached, so a refresh would repeat on every keystroke).
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.com")
	refreshAnthropicCompletionCache("anthropic:claude-", cfg)
	if calls != 1 {
		t.Fatalf("refreshed through ANTHROPIC_BASE_URL gateway: %d", calls)
	}
}

func TestUsageProviderFlagCompletionIncludesOpenRouter(t *testing.T) {
	completions, directive := UsageProviderFlagCompletion(nil, nil, "openr")
	if len(completions) != 1 || completions[0] != "openrouter" {
		t.Fatalf("completions = %v", completions)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v", directive)
	}
}
