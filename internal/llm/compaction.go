package llm

import (
	"context"
	"errors"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
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

// NativeCompact asks a provider that owns its conversation state to compact it
// in place. The outcomes:
//
//   - ok=true, err=nil: compacted natively.
//   - ok=true, err!=nil: cancelled. When native compaction returns an error,
//     cancellation (the caller's context, or a provider Canceled/
//     DeadlineExceeded) wins over every fallback. Do not fall back.
//   - ok=false: fall back to SoftCompact/Compact. The provider has no native
//     compaction, cannot compact right now, or native compaction failed.
//
// Falling back on failure matters because native compaction runs against the
// provider's own session, which can be exactly what needs rescuing: Claude Code
// refuses to compact a single oversized exchange. term-llm's transcript
// compaction does not depend on that session.
func NativeCompact(ctx context.Context, provider core.Provider, req core.NativeCompactRequest) (result *core.NativeCompactResult, ok bool, err error) {
	compactor, isCompactor := provider.(core.NativeCompactor)
	if !isCompactor {
		return nil, false, nil
	}
	result, err = compactor.NativeCompact(ctx, req)
	switch {
	case err == nil:
		return result, true, nil
	case ctx.Err() != nil:
		return nil, true, ctx.Err()
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return nil, true, err
	case errors.Is(err, core.ErrNativeCompactUnavailable):
		return nil, false, nil
	default:
		runtimeoutput.Warn("native compaction failed; falling back to transcript compaction", "error", err)
		return nil, false, nil
	}
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
