package cmd

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

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
	p.mu.Lock()
	if len(p.produced) == 0 && resultText != "" {
		p.produced = append(p.produced, tagResponseRunMessage(runCtx, llm.AssistantText(resultText), 0))
	}
	hasProduced := len(p.produced) > 0
	p.mu.Unlock()
	if !hasProduced {
		if p.replaceHistory && !p.initialPersisted {
			restoreReplaceHistory()
		}
		return
	}
	deferCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if p.appendOnlyPersisted {
		p.mu.Lock()
		p.updateStateAndAppendLocked(deferCtx)
		caughtUp := p.appendOnlyCaughtUpLocked()
		p.mu.Unlock()
		if caughtUp {
			p.rt.historyPersisted = true
			return
		}
	}
	p.persistProducedSnapshot(deferCtx)
}

// finalizeSuccess reconciles the successful transcript and accounting after the
// engine stream has terminated. It cannot alter the first run error because it
// is called only on the success path.
func (p *serveRunPersistence) finalizeSuccess(ctx, runCtx context.Context, req llm.Request, result serveRunResult) serveRunResult {
	p.rt.cumulativeUsage.Add(p.compactionUsageSnapshot())
	p.rt.cumulativeUsage.Add(result.Usage)
	result.SessionUsage = p.rt.cumulativeUsage
	p.mu.Lock()
	history := p.buildSnapshotLocked()
	synthesized := len(p.produced) == 0 && result.Text.Len() > 0
	if synthesized {
		assistant := tagResponseRunMessage(runCtx, llm.AssistantText(result.Text.String()), 0)
		history = append(history, assistant)
		if p.appendOnlyPersisted {
			p.produced = append(p.produced, assistant)
		}
	}
	if p.stateful {
		p.rt.history = history
		p.rt.historyPersisted = false
		p.rt.refreshSideQuestionSnapshot(history)
		p.rt.updateSideQuestionConfig(req)
	}
	needSnapshot, compacted := false, false
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
		compacted = p.compactedActiveHistory
	}
	p.persistPlatformInjectionLocked()
	p.mu.Unlock()
	if needSnapshot {
		if compacted {
			p.rt.historyPersisted = p.rt.persistCompactedSnapshot(ctx, p.sessionID, history)
		} else {
			p.rt.historyPersisted = p.rt.persistSnapshot(ctx, p.sessionID, history)
		}
	} else if p.persisted {
		p.rt.historyPersisted = true
	}
	if p.injectedPlatform != "" && p.stateful {
		p.rt.persistPlatformOrigin(ctx, p.sessionID, p.injectedPlatform)
	}
	if p.persisted && p.stateful {
		p.rt.persistProviderState(ctx, p.sessionID)
	}
	cached := result.SessionUsage.CachedInputTokens
	if p.rt.sessionMeta != nil && p.rt.sessionMeta.ID == p.sessionID && p.rt.sessionMeta.CachedInputTokens > cached {
		cached = p.rt.sessionMeta.CachedInputTokens
	}
	result.ContextUsage = contextUsageSnapshot(p.rt.engine, history, cached)
	return result
}
