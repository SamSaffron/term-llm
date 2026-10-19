package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/debug"
)

// Cobra callers retain these adapters until their command implementations move.
func loadConfig() (*config.Config, error)          { return bootstrap.LoadConfig() }
func registerModelLimits(cfg *config.Config)       { bootstrap.RegisterModelLimits(cfg) }
func loadConfigWithSetup() (*config.Config, error) { return bootstrap.LoadConfigWithSetup() }
func applyProviderOverrides(cfg *config.Config, provider, model, providerFlag string) error {
	return bootstrap.ApplyProviderOverrides(cfg, provider, model, providerFlag)
}
func applyProviderOverridesWithAgent(cfg *config.Config, cmdProvider, cmdModel, providerFlag, agentProvider, agentModel string) error {
	return bootstrap.ApplyProviderOverridesWithAgent(cfg, cmdProvider, cmdModel, providerFlag, agentProvider, agentModel)
}
func initThemeFromConfig(cfg *config.Config) { bootstrap.InitThemeFromConfig(cfg) }
func resolveForceExternalSearch(cfg *config.Config, nativeSearchFlag, noNativeSearchFlag bool) bool {
	return bootstrap.ResolveForceExternalSearch(cfg, nativeSearchFlag, noNativeSearchFlag)
}
func createDebugLogger(cfg *config.Config) (*debug.Logger, error) {
	return bootstrap.CreateDebugLogger(cfg)
}
func activeModel(cfg *config.Config) string { return bootstrap.ActiveModel(cfg) }
func generateSessionID() string             { return bootstrap.GenerateSessionID() }
