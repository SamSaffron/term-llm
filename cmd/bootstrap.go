package cmd

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	return cfg, nil
}

func loadConfigWithSetup() (*config.Config, error) {
	if config.NeedsSetup() {
		cfg, err := ui.RunSetupWizard()
		if err != nil {
			return nil, fmt.Errorf("setup cancelled: %w", err)
		}
		return cfg, nil
	}

	return loadConfig()
}

func applyProviderOverrides(cfg *config.Config, provider, model, providerFlag string) error {
	cfg.ApplyOverrides(provider, model)

	if providerFlag == "" {
		return nil
	}

	overrideProvider, overrideModel, err := llm.ParseProviderModel(providerFlag, cfg)
	if err != nil {
		return err
	}
	cfg.ApplyOverrides(overrideProvider, overrideModel)
	return nil
}

func initThemeFromConfig(cfg *config.Config) {
	ui.InitTheme(ui.ThemeConfig{
		Primary:   cfg.Theme.Primary,
		Secondary: cfg.Theme.Secondary,
		Success:   cfg.Theme.Success,
		Error:     cfg.Theme.Error,
		Warning:   cfg.Theme.Warning,
		Muted:     cfg.Theme.Muted,
		Text:      cfg.Theme.Text,
		Spinner:   cfg.Theme.Spinner,
	})
}

// resolveForceExternalSearch determines whether to force external search based on
// CLI flags and provider config. Flag values take precedence over config.
// nativeSearchFlag: true if --native-search was explicitly set
// noNativeSearchFlag: true if --no-native-search was explicitly set
func resolveForceExternalSearch(cfg *config.Config, nativeSearchFlag, noNativeSearchFlag bool) bool {
	// CLI flags take precedence
	if noNativeSearchFlag {
		return true // force external
	}
	if nativeSearchFlag {
		return false // force native
	}

	// Fall back to config
	providerCfg := cfg.GetActiveProviderConfig()
	if providerCfg != nil && providerCfg.UseNativeSearch != nil {
		return !*providerCfg.UseNativeSearch // if UseNativeSearch is false, force external
	}

	// Default: use native if provider supports it (don't force external)
	return false
}
