package llm

import (
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/providers/clibin"
)

// The CLI-binary provider family lives in providers/clibin.

func init() {
	// The argument-preview formatter lives with the engine.
	clibin.ToolInfoFormatter = engine.ExtractToolInfo
}

var (
	_ engine.ToolExecutorSetter  = (*clibin.AgyBinProvider)(nil)
	_ engine.ProviderCleaner     = (*clibin.AgyBinProvider)(nil)
	_ engine.ProviderTurnCleaner = (*clibin.AgyBinProvider)(nil)
	_ engine.ToolExecutorSetter  = (*clibin.ClaudeBinProvider)(nil)
	_ engine.ProviderCleaner     = (*clibin.ClaudeBinProvider)(nil)
	_ engine.ProviderTurnCleaner = (*clibin.ClaudeBinProvider)(nil)
	_ engine.ToolExecutorSetter  = (*clibin.CursorBinProvider)(nil)
	_ engine.ProviderCleaner     = (*clibin.CursorBinProvider)(nil)
	_ engine.ProviderTurnCleaner = (*clibin.CursorBinProvider)(nil)
	_ engine.ToolExecutorSetter  = (*clibin.GrokBinProvider)(nil)
	_ engine.ProviderCleaner     = (*clibin.GrokBinProvider)(nil)
	_ engine.ProviderTurnCleaner = (*clibin.GrokBinProvider)(nil)
)
