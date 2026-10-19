package llm

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
)

// compactionEnv adapts root-owned model limits, provider isolation, and
// conversation anchors to the engine package's compaction implementation.
var compactionEnv = engine.CompactionEnv{
	InputLimitForModel:      InputLimitForModel,
	ClampOutputTokens:       ClampOutputTokens,
	IsolateProvider:         isolatedConversationProvider,
	ForkProvider:            forkConversationProvider,
	ConversationStartFrom:   ConversationStartFrom,
	InsertConversationStart: InsertConversationStart,
	PlatformContextFrom:     PlatformContextFrom,
	InsertPlatformContext:   InsertPlatformContext,
}

func prepareCompactionContext(messages []core.Message, config engine.CompactionConfig, skipBrief string) engine.PreparedCompactionContext {
	return compactionEnv.PrepareCompactionContext(messages, config, skipBrief)
}

func compactionResultFromBriefPrepared(systemPrompt, brief string, prepared engine.PreparedCompactionContext, originalCount int, config engine.CompactionConfig) *engine.CompactionResult {
	return compactionEnv.CompactionResultFromBriefPrepared(systemPrompt, brief, prepared, originalCount, config)
}

// CompactionResultFromBrief builds a normal CompactionResult from a brief.
// It binds the root compaction environment (model limits, provider isolation).
func CompactionResultFromBrief(systemPrompt, brief string, messages []core.Message, config engine.CompactionConfig) *engine.CompactionResult {
	return compactionEnv.CompactionResultFromBrief(systemPrompt, brief, messages, config)
}

// SoftCompact performs the one LLM call needed for manual soft compaction.
// It binds the root compaction environment.
func SoftCompact(ctx context.Context, provider core.Provider, model, systemPrompt string, messages []core.Message, config engine.CompactionConfig) (*engine.CompactionResult, error) {
	return compactionEnv.SoftCompact(ctx, provider, model, systemPrompt, messages, config)
}

// Compact generates a structured continuation brief for the older prefix.
// It binds the root compaction environment.
func Compact(ctx context.Context, provider core.Provider, model, systemPrompt string, messages []core.Message, config engine.CompactionConfig) (*engine.CompactionResult, error) {
	return compactionEnv.Compact(ctx, provider, model, systemPrompt, messages, config)
}

// Handover produces a structured briefing for a different agent.
// It binds the root compaction environment.
func Handover(ctx context.Context, provider core.Provider, model, currentSystemPrompt, newSystemPrompt string, messages []core.Message, sourceAgent, targetAgent string, config engine.CompactionConfig, options engine.HandoverOptions) (*engine.HandoverResult, error) {
	return compactionEnv.Handover(ctx, provider, model, currentSystemPrompt, newSystemPrompt, messages, sourceAgent, targetAgent, config, options)
}
