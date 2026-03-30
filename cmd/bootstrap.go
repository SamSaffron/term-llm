package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	registerModelLimits(cfg)
	return cfg, nil
}

// registerModelLimits registers per-model token limits from provider configs
// so that InputLimitForModel/OutputLimitForModel work for custom models.
func registerModelLimits(cfg *config.Config) {
	// Register provider name → type aliases so provider-scoped limits
	// resolve correctly for custom names (e.g., "acme" → "venice").
	aliases := make(map[string]string)
	for name, pc := range cfg.Providers {
		if pc.Type != "" {
			aliases[name] = string(pc.Type)
		}
	}
	llm.RegisterProviderAliases(aliases)

	var limits []llm.ConfigModelLimit
	for name, pc := range cfg.Providers {
		if pc.Model == "" {
			continue
		}
		if pc.ContextWindow <= 0 && pc.MaxOutputTokens <= 0 {
			continue
		}

		var inputLimit, outputLimit int
		switch {
		case pc.ContextWindow > 0 && pc.MaxOutputTokens > 0:
			inputLimit = pc.ContextWindow - pc.MaxOutputTokens
			outputLimit = pc.MaxOutputTokens
			if inputLimit <= 0 {
				continue // nonsensical config, skip
			}
		case pc.ContextWindow > 0:
			// Only context_window set: use full window as input limit.
			// Output clamping won't apply — compaction fires but output
			// tokens are uncapped. Set max_output_tokens for tighter control.
			inputLimit = pc.ContextWindow
		case pc.MaxOutputTokens > 0:
			outputLimit = pc.MaxOutputTokens
		}

		limits = append(limits, llm.ConfigModelLimit{
			Provider:    name,
			Model:       pc.Model,
			InputLimit:  inputLimit,
			OutputLimit: outputLimit,
		})
	}
	// Always register (even if empty) so a config reload clears stale limits.
	llm.RegisterConfigLimits(limits)
}

func loadConfigWithSetup() (*config.Config, error) {
	if config.NeedsSetup() {
		cfg, err := ui.RunSetupWizard()
		if err != nil {
			return nil, fmt.Errorf("setup cancelled: %w", err)
		}
		registerModelLimits(cfg)
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

// applyProviderOverridesWithAgent applies provider overrides with agent settings.
// Priority: CLI flag > agent settings > command-specific config > global config
func applyProviderOverridesWithAgent(cfg *config.Config, cmdProvider, cmdModel, providerFlag, agentProvider, agentModel string) error {
	// Start with command-specific config (e.g., ask.provider, ask.model)
	cfg.ApplyOverrides(cmdProvider, cmdModel)

	// Apply agent settings if present (lower priority than CLI)
	if providerFlag == "" {
		if agentProvider != "" {
			cfg.ApplyOverrides(agentProvider, "")
		}
		if agentModel != "" {
			cfg.ApplyOverrides("", agentModel)
		}
	}

	// CLI flag has highest priority
	if providerFlag != "" {
		overrideProvider, overrideModel, err := llm.ParseProviderModel(providerFlag, cfg)
		if err != nil {
			return err
		}
		cfg.ApplyOverrides(overrideProvider, overrideModel)
	}

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
// CLI flags, global search config, and provider config.
// Precedence: CLI flags > global search.force_external > per-provider use_native_search > default
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

	// Global search config override
	if cfg.Search.ForceExternal {
		return true
	}

	// Fall back to per-provider config
	providerCfg := cfg.GetActiveProviderConfig()
	if providerCfg != nil && providerCfg.UseNativeSearch != nil {
		return !*providerCfg.UseNativeSearch // if UseNativeSearch is false, force external
	}

	// Default: use native if provider supports it (don't force external)
	return false
}

// createDebugLogger creates a debug logger if debug_logs is enabled in config.
// Returns nil if debug logging is disabled.
// Returns an error if debug logging is enabled but logger creation fails.
// The caller is responsible for calling Close() on the returned logger when done.
func createDebugLogger(cfg *config.Config) (*llm.DebugLogger, error) {
	if !cfg.DebugLogs.Enabled {
		return nil, nil
	}

	// Use configured dir or default
	dir := cfg.DebugLogs.Dir
	if dir == "" {
		dir = config.GetDebugLogsDir()
	}

	// Generate session ID: timestamp + random suffix for uniqueness
	sessionID := generateSessionID()

	logger, err := llm.NewDebugLogger(dir, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to create debug logger: %w", err)
	}

	// Log session start with CLI invocation
	command := filepath.Base(os.Args[0])
	args := os.Args[1:]
	cwd, _ := os.Getwd()
	logger.LogSessionStart(command, args, cwd)

	return logger, nil
}

// activeModel returns the model name from the active provider config, or empty string.
func activeModel(cfg *config.Config) string {
	if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
		return providerCfg.Model
	}
	return ""
}

// generateSessionID creates a unique session identifier using timestamp and random bytes.
// Format: 2006-01-02T15-04-05-abc123
func generateSessionID() string {
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		// Fallback to timestamp only if random fails.
		// crypto/rand failures are rare and usually indicate serious system issues,
		// but debug logging is non-critical so we gracefully degrade.
		return timestamp
	}
	return fmt.Sprintf("%s-%s", timestamp, hex.EncodeToString(suffix))
}
