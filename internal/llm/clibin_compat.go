package llm

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm/providers/clibin"
)

// Compatibility exports for the CLI-binary provider family, which now lives in
// providers/clibin.

type (
	AgyBinProvider          = clibin.AgyBinProvider
	ClaudeBinProvider       = clibin.ClaudeBinProvider
	ClaudeCommandError      = clibin.ClaudeCommandError
	CursorBinProvider       = clibin.CursorBinProvider
	GrokBinProvider         = clibin.GrokBinProvider
	UserFacingProviderError = clibin.UserFacingProviderError
	CLICommandError         = clibin.CLICommandError
)

func init() {
	// The argument-preview formatter lives with the engine.
	clibin.ToolInfoFormatter = ExtractToolInfo
}

var (
	_ ToolExecutorSetter  = (*AgyBinProvider)(nil)
	_ ProviderCleaner     = (*AgyBinProvider)(nil)
	_ ProviderTurnCleaner = (*AgyBinProvider)(nil)
	_ ToolExecutorSetter  = (*ClaudeBinProvider)(nil)
	_ ProviderCleaner     = (*ClaudeBinProvider)(nil)
	_ ProviderTurnCleaner = (*ClaudeBinProvider)(nil)
	_ ToolExecutorSetter  = (*CursorBinProvider)(nil)
	_ ProviderCleaner     = (*CursorBinProvider)(nil)
	_ ProviderTurnCleaner = (*CursorBinProvider)(nil)
	_ ToolExecutorSetter  = (*GrokBinProvider)(nil)
	_ ProviderCleaner     = (*GrokBinProvider)(nil)
	_ ProviderTurnCleaner = (*GrokBinProvider)(nil)
)

func NewAgyBinProvider(model string, env map[string]string) *AgyBinProvider {
	return clibin.NewAgyBinProvider(model, env)
}
func ValidateAgyBinModel(model string) error { return clibin.ValidateAgyBinModel(model) }
func AgyBinHasCredentials() bool             { return clibin.AgyBinHasCredentials() }
func RefreshAgyBinModelsIfStale(ctx context.Context, model string, env map[string]string) error {
	return clibin.RefreshAgyBinModelsIfStale(ctx, model, env)
}
func GetCachedAgyBinModels() []string           { return clibin.GetCachedAgyBinModels() }
func GetCachedAgyBinModelInfos() []ModelInfo    { return clibin.GetCachedAgyBinModelInfos() }
func RefreshAgyBinCacheSync(models []ModelInfo) { clibin.RefreshAgyBinCacheSync(models) }
func AgyVersionAtLeast(version string, wantMajor, wantMinor, wantPatch int) bool {
	return clibin.AgyVersionAtLeast(version, wantMajor, wantMinor, wantPatch)
}

func ValidateClaudeBinModel(model string) error { return clibin.ValidateClaudeBinModel(model) }
func NewClaudeBinProvider(model string, env map[string]string) *ClaudeBinProvider {
	return clibin.NewClaudeBinProvider(model, env)
}

func ValidateCursorBinModel(model string) error { return clibin.ValidateCursorBinModel(model) }
func NewCursorBinProvider(model string, env map[string]string) *CursorBinProvider {
	return clibin.NewCursorBinProvider(model, env)
}
func CursorBinHasCredentials() bool                { return clibin.CursorBinHasCredentials() }
func GetCachedCursorBinModels() []string           { return clibin.GetCachedCursorBinModels() }
func GetCachedCursorBinModelInfos() []ModelInfo    { return clibin.GetCachedCursorBinModelInfos() }
func RefreshCursorBinCacheSync(models []ModelInfo) { clibin.RefreshCursorBinCacheSync(models) }

func ValidateGrokBinModel(model string) error { return clibin.ValidateGrokBinModel(model) }
func NewGrokBinProvider(model string, env map[string]string) *GrokBinProvider {
	return clibin.NewGrokBinProvider(model, env)
}
func RefreshGrokBinModelsIfStale(ctx context.Context, model string, env map[string]string) error {
	return clibin.RefreshGrokBinModelsIfStale(ctx, model, env)
}
func GetCachedGrokBinModels() []string           { return clibin.GetCachedGrokBinModels() }
func GetCachedGrokBinModelInfos() []ModelInfo    { return clibin.GetCachedGrokBinModelInfos() }
func RefreshGrokBinCacheSync(models []ModelInfo) { clibin.RefreshGrokBinCacheSync(models) }

// grokBinAuthPath returns the Grok Build CLI login file.
func grokBinAuthPath(override string) string { return clibin.GrokBinAuthPath(override) }
