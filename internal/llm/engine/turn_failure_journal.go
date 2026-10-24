package engine

import "github.com/samsaffron/term-llm/internal/llm/core"

// journalFailedSyncTurn preserves outcomes already committed by an inline tool
// provider even when its terminal error is not eligible for automatic recovery.
// It never executes tools or retries a provider stream. Explicit continuation
// can then hydrate real results rather than synthesizing "side effects unknown".
func (e *Engine) journalFailedSyncTurn(r *engineTurnRecovery) {
	if !*r.syncToolsExecuted || r.turnCallback == nil {
		return
	}
	r.settleSyncTools()
	if len(*r.syncToolResults) == 0 {
		return
	}
	var assistant core.Message
	if r.preserveInlineToolOrder && len(*r.inlineSyncParts) > 0 {
		assistant = r.buildOrderedInlineAssistant(*r.inlineSyncParts)
	} else {
		assistant = buildAssistantMessageWithReasoningMetadata(r.textBuilder.String(), e.withToolPreview(*r.syncToolCalls), r.reasoningBuilder.String(), *r.reasoningSummaryParts, *r.reasoningItemID, *r.reasoningEncryptedContent, *r.reasoningKind)
	}
	assistant = attachProviderReplayParts(assistant, *r.providerReplayParts)
	messages := append([]core.Message{assistant}, (*r.syncToolResults)...)
	metrics := *r.turnMetrics
	metrics.ToolCalls = len(*r.syncToolCalls)
	ctx, cancel := callbackContext(r.ctx)
	defer cancel()
	_ = r.turnCallback(ctx, r.attempt, messages, metrics)
}
