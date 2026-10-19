package runtime

import (
	"context"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

// RecordStoreModelUsage attributes a share of a session's durable totals to the
// model that spent it. Call it only alongside UpdateMetrics.
func RecordStoreModelUsage(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u core.Usage, llmTurns, toolCalls int) {
	if store == nil || strings.TrimSpace(sessionID) == "" || u.BillableCountersZero() {
		return
	}
	_ = store.RecordModelUsage(ctx, sessionID, session.ModelUsage{
		Model: model, Kind: kind, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CachedInputTokens: u.CachedInputTokens, CacheWriteTokens: u.CacheWriteTokens,
		LLMTurns: llmTurns, ToolCalls: toolCalls,
	})
}

// RecordStoreTurnUsage advances aggregate turn metrics and attributes the same
// work to the model only when the aggregate update succeeds.
func RecordStoreTurnUsage(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u core.Usage, toolCalls int) {
	if store == nil {
		return
	}
	if err := store.UpdateMetrics(ctx, sessionID, 1, toolCalls, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens); err != nil {
		return
	}
	RecordStoreModelUsage(ctx, store, sessionID, model, kind, u, 1, toolCalls)
}

// RecordStoreHelperUsage records helper spend without advancing turn counters.
func RecordStoreHelperUsage(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u core.Usage) {
	if store == nil {
		return
	}
	if err := store.UpdateMetrics(ctx, sessionID, 0, 0, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens); err != nil {
		return
	}
	RecordStoreModelUsage(ctx, store, sessionID, model, kind, u, 1, 0)
}
