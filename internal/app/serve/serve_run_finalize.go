package serve

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// reconcilePersistedRunError runs under rt.mu, after the fenced error write.
func (rt *serveRuntime) reconcilePersistedRunError(msg core.Message) {
	if rt.historyPersisted {
		rt.history = append(rt.history, msg)
		return
	}
	// An unhydrated or divergent snapshot cannot become authoritative just by
	// appending the marker. Force the next turn to reload the durable transcript.
	rt.history = nil
}

func (p *serveRunPersistence) reconcileFailure(ctx, runCtx context.Context, runErr error, resultText string, restoreReplaceHistory func()) {
	if runErr == nil {
		return
	}
	if !p.persisted {
		if p.replaceHistory {
			restoreReplaceHistory()
		}
		return
	}
	// Native CLI sessions may have committed tools before returning an error.
	// Persist their resume cursor even for the initial, non-stateful child run.
	defer p.rt.persistProviderState(ctx, p.sessionID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.produced) == 0 && resultText != "" {
		p.produced = append(p.produced, tagResponseRunMessage(runCtx, core.AssistantText(resultText), 0))
	}
	if len(p.produced) == 0 && p.replaceHistory && !p.initialPersisted {
		restoreReplaceHistory()
		return
	}
	deferCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if len(p.produced) > 0 {
		p.persistFailureOutputLocked(deferCtx)
	}
	p.persistPlatformInjectionLocked()
	if p.stateful {
		// Durable callbacks may have advanced beyond this run's snapshot. Use
		// the stored active context, or leave hydration pending if loading fails.
		p.rt.history = nil
		p.rt.historyPersisted = false
		p.rt.restorePersistedHistory(deferCtx, p.rt.sessionMeta)
	}
}

// persistFailureOutputLocked retries only uncommitted input/output and a pending
// assistant upsert. A failed run never owns a transcript replacement: even a
// seemingly complete snapshot can be stale after incremental writes or a swap.
func (p *serveRunPersistence) persistFailureOutputLocked(ctx context.Context) {
	if !p.initialPersisted && !p.appendInitialLocked(ctx) {
		return
	}
	if (p.assistantSnapshotDirty || p.assistantSnapshotNeedsReconcile) && p.pendingAssistantIdx >= 0 && p.pendingAssistantIdx < len(p.produced) {
		p.upsertAssistantLocked(ctx, p.produced[p.pendingAssistantIdx], true)
		if p.assistantSnapshotDirty {
			return
		}
	}
	if p.lastAppendedIdx < len(p.produced) {
		result := p.rt.appendMessagesDetailed(ctx, p.sessionID, p.produced[p.lastAppendedIdx:], p.turnIndex)
		p.lastAppendedIdx += result.Written
	}
}

// finalizeSuccess reconciles the successful transcript and accounting after the
// engine stream has terminated. It cannot alter the first run error because it
// is called only on the success path.
func (p *serveRunPersistence) finalizeSuccess(ctx, runCtx context.Context, req core.Request, result serveRunResult) serveRunResult {
	p.rt.cumulativeUsage.Add(p.compactionUsageSnapshot())
	p.rt.cumulativeUsage.Add(result.Usage)
	result.SessionUsage = p.rt.cumulativeUsage
	p.mu.Lock()
	history := p.buildSnapshotLocked()
	synthesized := len(p.produced) == 0 && result.Text.Len() > 0
	if synthesized {
		assistant := tagResponseRunMessage(runCtx, core.AssistantText(result.Text.String()), 0)
		history = append(history, assistant)
		if p.appendOnlyPersisted {
			p.produced = append(p.produced, assistant)
		}
	}
	if p.stateful {
		p.rt.history = history
		p.rt.historyPersisted = false
		p.rt.refreshSideQuestionSnapshot(p.rt.sideQuestionBoundary(history, 0, false))
		p.rt.updateSideQuestionConfig(req)
	}
	needSnapshot, reset := false, false
	if p.persisted {
		if p.appendOnlyPersisted {
			if (p.assistantSnapshotDirty || p.assistantSnapshotNeedsReconcile) && p.pendingAssistantIdx >= 0 && p.pendingAssistantIdx < len(p.produced) {
				p.upsertAssistantLocked(ctx, p.produced[p.pendingAssistantIdx], true)
			}
			p.updateStateAndAppendLocked(ctx)
			needSnapshot = !p.appendOnlyCaughtUpLocked()
		} else {
			needSnapshot = !p.initialPersisted || p.lastAppendedIdx < len(p.produced) || p.assistantSnapshotDirty || p.assistantSnapshotNeedsReconcile || synthesized
		}
		reset = p.resetTranscript
	}
	p.persistPlatformInjectionLocked()
	p.mu.Unlock()
	if needSnapshot {
		p.rt.historyPersisted = p.rt.persistSnapshotWithInitialBoundary(ctx, p.sessionID, history, reset, false)
	} else if p.persisted {
		p.rt.historyPersisted = true
	}
	if p.injectedPlatform != "" && p.stateful {
		p.rt.persistPlatformOrigin(ctx, p.sessionID, p.injectedPlatform)
	}
	if p.persisted {
		p.rt.persistProviderState(ctx, p.sessionID)
	}
	cached := result.SessionUsage.CachedInputTokens
	if p.rt.sessionMeta != nil && p.rt.sessionMeta.ID == p.sessionID && p.rt.sessionMeta.CachedInputTokens > cached {
		cached = p.rt.sessionMeta.CachedInputTokens
	}
	result.ContextUsage = contextUsageSnapshot(p.rt.engine, history, cached)
	return result
}
