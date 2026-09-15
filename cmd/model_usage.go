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
