package cmd

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// serveRunPersistence owns transcript state shared by engine callbacks during
// one runOnce invocation. Its lifetime ends after final reconciliation. It does
// not own the runtime lock, checkout lease, activity reservation, engine stream,
// cancellation, or callback installation/removal.
type serveRunSpec struct {
	stateful       bool
	persisted      bool
	replaceHistory bool
	batchInitial   bool
}

type serveRunPersistence struct {
	rt                                  *serveRuntime
	sessionID                           string
	turnIndex                           int
	stateful, persisted, replaceHistory bool
	batchInitial                        bool
	injectedPlatform                    string

	mu                                                      sync.Mutex
	baseHistory, inputMessages, produced                    []llm.Message
	systemPromptInjected                                    bool
	lastAppendedIdx                                         int
	assistantSnapshotDirty, assistantSnapshotNeedsReconcile bool
	pendingAssistantIdx                                     int
	pendingAssistantMsgID                                   int64
	pendingAssistantTextPersisted                           bool
	compactedActiveHistory                                  bool
	appendOnlyPersisted, initialPersisted                   bool
	initialMessages                                         []llm.Message
	initialAppendedIdx                                      int
	lastAppendResult                                        appendMessagesResult

	compactionUsageMu sync.Mutex
	compactionUsage   llm.Usage
}

func newServeRunPersistence(rt *serveRuntime, sessionID string, turnIndex int, spec serveRunSpec, baseHistory, inputMessages []llm.Message, systemPromptInjected bool, injectedPlatform string) *serveRunPersistence {
	p := &serveRunPersistence{rt: rt, sessionID: sessionID, turnIndex: turnIndex, stateful: spec.stateful, persisted: spec.persisted, replaceHistory: spec.replaceHistory, batchInitial: spec.batchInitial, baseHistory: baseHistory, inputMessages: inputMessages, systemPromptInjected: systemPromptInjected, injectedPlatform: injectedPlatform, pendingAssistantIdx: -1}
	p.appendOnlyPersisted = spec.persisted && !spec.replaceHistory && rt.historyPersisted && (!isIdentifiedUserBatch(inputMessages) || spec.batchInitial)
	if systemPromptInjected {
		p.initialMessages = append(p.initialMessages, llm.SystemText(rt.systemPrompt))
	}
	p.initialMessages = append(p.initialMessages, inputMessages...)
	return p
}
func (p *serveRunPersistence) persistPlatformInjectionLocked() {
	if p.injectedPlatform != "" && (p.stateful || p.persisted) {
		p.rt.lastInjectedPlatform = p.injectedPlatform
	}
}
func (p *serveRunPersistence) buildSnapshotLocked() []llm.Message {
	out := make([]llm.Message, 0, len(p.baseHistory)+len(p.inputMessages)+len(p.produced)+1)
	if p.systemPromptInjected {
		out = append(out, llm.SystemText(p.rt.systemPrompt))
	}
	out = append(out, p.baseHistory...)
	out = append(out, p.inputMessages...)
	out = append(out, p.produced...)
	return out
}
func (p *serveRunPersistence) snapshot() []llm.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buildSnapshotLocked()
}
func (p *serveRunPersistence) persistProducedSnapshot(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := p.buildSnapshotLocked()
	if p.stateful {
		p.rt.history = snapshot
		p.rt.historyPersisted = false
	}
	if p.persisted {
		if p.compactedActiveHistory {
			p.rt.historyPersisted = p.rt.persistCompactedSnapshot(ctx, p.sessionID, snapshot)
		} else {
			p.rt.historyPersisted = p.rt.persistSnapshot(ctx, p.sessionID, snapshot)
		}
	}
	p.persistPlatformInjectionLocked()
}
func (p *serveRunPersistence) appendOnlyCaughtUpLocked() bool {
	return p.appendOnlyPersisted && p.initialPersisted && p.initialAppendedIdx >= len(p.initialMessages) && p.lastAppendedIdx >= len(p.produced) && !p.assistantSnapshotDirty && !p.assistantSnapshotNeedsReconcile
}
func (p *serveRunPersistence) appendInitialLocked(ctx context.Context) bool {
	if !p.appendOnlyPersisted || p.initialAppendedIdx >= len(p.initialMessages) {
		p.initialPersisted = true
		return true
	}
	var result appendMessagesResult
	if p.batchInitial {
		result = p.rt.appendMessagesBatchDetailed(ctx, p.sessionID, p.initialMessages[p.initialAppendedIdx:], p.turnIndex)
	} else {
		result = p.rt.appendMessagesDetailed(ctx, p.sessionID, p.initialMessages[p.initialAppendedIdx:], p.turnIndex)
	}
	p.lastAppendResult = result
	p.initialAppendedIdx += result.Written
	if p.initialAppendedIdx < len(p.initialMessages) {
		p.appendOnlyPersisted = false
		p.rt.historyPersisted = false
		return false
	}
	p.initialPersisted = true
	return true
}
func (p *serveRunPersistence) persistInitial(ctx context.Context, replacingExisting bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.persisted || (p.replaceHistory && replacingExisting) {
		return
	}
	if p.appendOnlyPersisted {
		p.appendInitialLocked(ctx)
		return
	}
	initial := make([]llm.Message, 0, len(p.baseHistory)+len(p.inputMessages)+1)
	if p.systemPromptInjected {
		initial = append(initial, llm.SystemText(p.rt.systemPrompt))
	}
	initial = append(initial, p.baseHistory...)
	initial = append(initial, p.inputMessages...)
	p.initialPersisted = p.rt.persistInitialSnapshot(ctx, p.sessionID, initial)
}
func (p *serveRunPersistence) updateStateAndAppendLocked(ctx context.Context) {
	if p.stateful {
		p.rt.history = p.buildSnapshotLocked()
		p.rt.historyPersisted = false
	}
	if p.persisted {
		if p.appendOnlyPersisted {
			if !p.appendInitialLocked(ctx) {
				p.persistPlatformInjectionLocked()
				return
			}
			if p.lastAppendedIdx < len(p.produced) {
				result := p.rt.appendMessagesDetailed(ctx, p.sessionID, p.produced[p.lastAppendedIdx:], p.turnIndex)
				p.lastAppendResult = result
				p.lastAppendedIdx += result.Written
				if p.lastAppendedIdx < len(p.produced) {
					p.appendOnlyPersisted = false
					p.rt.historyPersisted = false
				}
			}
			p.persistPlatformInjectionLocked()
			return
		}
		if !p.initialPersisted {
			initial := make([]llm.Message, 0, len(p.baseHistory)+len(p.inputMessages)+1)
			if p.systemPromptInjected {
				initial = append(initial, llm.SystemText(p.rt.systemPrompt))
			}
			initial = append(initial, p.baseHistory...)
			initial = append(initial, p.inputMessages...)
			p.initialPersisted = p.rt.persistInitialSnapshot(ctx, p.sessionID, initial)
		}
		if p.initialPersisted && p.lastAppendedIdx < len(p.produced) {
			result := p.rt.appendMessagesDetailed(ctx, p.sessionID, p.produced[p.lastAppendedIdx:], p.turnIndex)
			p.lastAppendResult = result
			p.lastAppendedIdx += result.Written
		}
	}
	p.persistPlatformInjectionLocked()
}
func (p *serveRunPersistence) upsertAssistantLocked(ctx context.Context, assistant llm.Message, finalize bool) {
	if p.pendingAssistantIdx < 0 {
		p.pendingAssistantIdx = len(p.produced)
		p.produced = append(p.produced, assistant)
		p.pendingAssistantTextPersisted = false
	} else {
		p.produced[p.pendingAssistantIdx] = assistant
	}
	p.assistantSnapshotDirty = true
	if p.stateful {
		p.rt.history = p.buildSnapshotLocked()
		p.rt.historyPersisted = false
	}
	if !p.persisted {
		p.persistPlatformInjectionLocked()
		return
	}
	if p.appendOnlyPersisted {
		if !p.appendInitialLocked(ctx) {
			p.persistPlatformInjectionLocked()
			return
		}
	} else if !p.initialPersisted {
		initial := make([]llm.Message, 0, len(p.baseHistory)+len(p.inputMessages)+1)
		if p.systemPromptInjected {
			initial = append(initial, llm.SystemText(p.rt.systemPrompt))
		}
		initial = append(initial, p.baseHistory...)
		initial = append(initial, p.inputMessages...)
		p.initialPersisted = p.rt.persistInitialSnapshot(ctx, p.sessionID, initial)
		if !p.initialPersisted {
			p.persistPlatformInjectionLocked()
			return
		}
	}
	dbCtx, cancel := inlinePersistContext(ctx, 10*time.Second)
	defer cancel()
	message := session.NewMessage(p.sessionID, assistant, -1)
	message.TurnIndex = p.turnIndex
	if p.pendingAssistantMsgID != 0 {
		message.ID = p.pendingAssistantMsgID
		_, err := runResponseRunPersistence(ctx, []llm.Message{assistant}, func(fence session.ResponseRunFence) (int64, error) {
			return updateResponseRunStreamingMessage(session.WithResponseRunFence(dbCtx, fence), p.rt.store, p.sessionID, message, finalize)
		})
		if err == nil {
			p.assistantSnapshotDirty = false
			if finalize {
				p.pendingAssistantTextPersisted = true
			}
			p.persistPlatformInjectionLocked()
			return
		}
		if !errors.Is(err, session.ErrNotFound) {
			p.assistantSnapshotNeedsReconcile = true
			p.appendOnlyPersisted = false
			p.rt.historyPersisted = false
			log.Printf("[serve] session UpdateMessage failed for %s: %v", p.sessionID, err)
			p.persistPlatformInjectionLocked()
			return
		}
		p.pendingAssistantMsgID = 0
		p.pendingAssistantTextPersisted = false
		message = session.NewMessage(p.sessionID, assistant, -1)
		message.TurnIndex = p.turnIndex
	}
	_, err := runResponseRunPersistence(ctx, []llm.Message{assistant}, func(fence session.ResponseRunFence) (int64, error) {
		return addResponseRunMessage(session.WithResponseRunFence(dbCtx, fence), p.rt.store, p.sessionID, message)
	})
	if err != nil {
		p.assistantSnapshotNeedsReconcile = true
		p.appendOnlyPersisted = false
		p.rt.historyPersisted = false
		log.Printf("[serve] session AddMessage failed for %s: %v", p.sessionID, err)
		p.persistPlatformInjectionLocked()
		return
	}
	p.pendingAssistantMsgID = message.ID
	p.pendingAssistantTextPersisted = finalize
	p.assistantSnapshotDirty = false
	if p.pendingAssistantIdx+1 > p.lastAppendedIdx {
		p.lastAppendedIdx = p.pendingAssistantIdx + 1
	}
	p.persistPlatformInjectionLocked()
}
func (p *serveRunPersistence) addCompactionUsage(usage llm.Usage) {
	p.compactionUsageMu.Lock()
	p.compactionUsage.Add(usage)
	p.compactionUsageMu.Unlock()
}
func (p *serveRunPersistence) compactionUsageSnapshot() llm.Usage {
	p.compactionUsageMu.Lock()
	defer p.compactionUsageMu.Unlock()
	return p.compactionUsage
}

func (p *serveRunPersistence) applyCompaction(cbCtx context.Context, result *llm.CompactionResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if result == nil {
		return nil
	}
	previousCompactionSeq := -1
	previousCompactionCount := 0
	if session.HasCompactionBoundary(p.rt.sessionMeta) {
		previousCompactionSeq = p.rt.sessionMeta.CompactionSeq
		previousCompactionCount = p.rt.sessionMeta.CompactionCount
	}
	handledByPlatform := p.rt.compactionCB != nil
	if handledByPlatform {
		if err := p.rt.compactionCB(cbCtx, result); err != nil {
			return err
		}
		// Platform callbacks own persistence, but the response stream still
		// needs the resulting durable boundary identity. Refresh the runtime
		// snapshot after the callback has committed it.
		if p.rt.store != nil && p.rt.sessionMeta != nil && p.rt.sessionMeta.ID != "" {
			if refreshed, err := p.rt.store.Get(cbCtx, p.rt.sessionMeta.ID); err == nil && refreshed != nil {
				p.rt.sessionMeta = refreshed
			}
		}
	}
	var compacted []llm.Message
	if handledByPlatform {
		// Platform callbacks persist only durable replacement history. Keep the
		// runtime snapshot durable too; Engine keeps ActiveMessages in its
		// in-flight request and restores ephemeral plan context on later streams.
		compacted = append(compacted, result.NewMessages...)
	} else {
		updated, _, refreshed, err := session.ApplyCompaction(cbCtx, p.rt.store, p.rt.sessionMeta, nil, result)
		if err != nil {
			return err
		}
		if refreshed != nil {
			p.rt.sessionMeta = refreshed
		}
		compacted = make([]llm.Message, 0, len(updated))
		for _, msg := range updated {
			compacted = append(compacted, msg.ToLLMMessage())
		}
	}
	if session.HasCompactionBoundary(p.rt.sessionMeta) &&
		(p.rt.sessionMeta.CompactionSeq != previousCompactionSeq || p.rt.sessionMeta.CompactionCount > previousCompactionCount) {
		p.rt.recordPendingCompactionIdentity(p.rt.sessionMeta.CompactionSeq, p.rt.sessionMeta.CompactionCount)
	}
	if !result.Usage.IsZero() {
		p.compactionUsageMu.Lock()
		p.compactionUsage.Add(result.Usage)
		p.compactionUsageMu.Unlock()
	}
	if !result.Usage.BillableCountersZero() && p.rt.store != nil && p.rt.sessionMeta != nil {
		if err := p.rt.store.UpdateMetrics(cbCtx, p.rt.sessionMeta.ID, 0, 0, result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.CachedInputTokens, result.Usage.CacheWriteTokens); err == nil {
			p.rt.sessionMeta.InputTokens += result.Usage.InputTokens
			p.rt.sessionMeta.OutputTokens += result.Usage.OutputTokens
			p.rt.sessionMeta.CachedInputTokens += result.Usage.CachedInputTokens
			p.rt.sessionMeta.CacheWriteTokens += result.Usage.CacheWriteTokens
		}
	}
	if !handledByPlatform && len(compacted) == 0 {
		compacted = append(compacted, result.NewMessages...)
	}
	p.baseHistory = compacted
	p.compactedActiveHistory = true
	p.inputMessages = nil
	p.produced = nil
	p.lastAppendedIdx = 0
	p.initialPersisted = p.persisted
	p.initialAppendedIdx = len(p.initialMessages)
	p.systemPromptInjected = false
	p.pendingAssistantIdx = -1
	p.pendingAssistantMsgID = 0
	p.pendingAssistantTextPersisted = false
	p.assistantSnapshotDirty = false
	p.assistantSnapshotNeedsReconcile = false
	if p.stateful {
		p.rt.history = append([]llm.Message(nil), compacted...)
		p.rt.historyPersisted = p.persisted
		p.rt.refreshSideQuestionSnapshot(compacted)
	}
	p.rt.engine.SetContextEstimateBaseline(0, 0)
	p.persistPlatformInjectionLocked()
	return nil
}

func (p *serveRunPersistence) persistRuntimeSwitch(cbCtx context.Context, change llm.RuntimeSwitch) error {
	marker := llm.ModelSwapMarker{
		FromProvider: runtimeProviderKey(p.rt),
		FromModel:    change.PreviousModel,
		FromEffort:   change.PreviousReasoningEffort,
		ToProvider:   runtimeProviderKey(p.rt),
		ToModel:      change.Model,
		ToEffort:     change.ReasoningEffort,
		BoundaryID:   change.BoundaryID,
		Status:       "succeeded",
	}
	msg := tagResponseRunMessage(cbCtx, llm.ModelSwapEventMessage(marker), -1)
	p.mu.Lock()
	defer p.mu.Unlock()
	markerIndex := len(p.produced)
	p.produced = append(p.produced, msg)
	p.updateStateAndAppendLocked(cbCtx)
	if p.persisted && markerIndex >= p.lastAppendedIdx {
		p.produced = p.produced[:markerIndex]
		if p.stateful {
			p.rt.history = p.buildSnapshotLocked()
		}
		return errors.New("model switch boundary was not durably persisted")
	}
	return nil
}
