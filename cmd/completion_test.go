package cmd

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
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
