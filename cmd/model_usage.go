package cmd

import (
	"context"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// recordStoreModelUsage attributes a share of a session's durable totals to the
// model that spent it.
//
// Call it only alongside UpdateMetrics. The per-model rows are meant to
// decompose the aggregate session bucket, so recording work that never entered
// that bucket would make the breakdown and the totals disagree.
func recordStoreModelUsage(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u llm.Usage, llmTurns, toolCalls int) {
	if store == nil || strings.TrimSpace(sessionID) == "" || u.BillableCountersZero() {
		return
	}
	_ = store.RecordModelUsage(ctx, sessionID, session.ModelUsage{
		Model:             model,
		Kind:              kind,
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CachedInputTokens,
		CacheWriteTokens:  u.CacheWriteTokens,
		LLMTurns:          llmTurns,
		ToolCalls:         toolCalls,
	})
}

// recordStoreTurnUsage records a main assistant turn: the aggregate session
// counters advance by one turn plus its tool calls, and the same work is
// attributed to the model that ran it. The per-model row is written only when
// the aggregate update lands, so the breakdown never claims usage the totals
// do not have.
func recordStoreTurnUsage(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u llm.Usage, toolCalls int) {
	if store == nil {
		return
	}
	if err := store.UpdateMetrics(ctx, sessionID, 1, toolCalls, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens); err != nil {
		return
	}
	recordStoreModelUsage(ctx, store, sessionID, model, kind, u, 1, toolCalls)
}

// recordStoreHelperUsage records guardian, compaction and similar helper work.
// Helpers spend tokens without advancing the session's own turn or tool
// counters, but they still own a model row so their spend is attributable.
func recordStoreHelperUsage(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u llm.Usage) {
	if store == nil {
		return
	}
	if err := store.UpdateMetrics(ctx, sessionID, 0, 0, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens); err != nil {
		return
	}
	recordStoreModelUsage(ctx, store, sessionID, model, kind, u, 1, 0)
}
