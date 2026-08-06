package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
)

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
