package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

var (
	errServeCompactionUnavailable = errors.New("session compaction is unavailable")
	errServeCompactionTooSmall    = errors.New("not enough conversation history to compress")
)

type preparedSessionCompaction struct {
	rt           *serveRuntime
	sessionID    string
	history      []core.Message
	systemPrompt string
	model        string
	config       engine.CompactionConfig
}

// prepareCompaction reserves both admission and the runtime mutex. The owner
// must call release, including if scheduling the prepared work fails.
func (rt *serveRuntime) prepareCompaction(ctx context.Context, sessionID string) (*preparedSessionCompaction, error) {
	if rt == nil {
		return nil, errServeCompactionUnavailable
	}
	if !rt.compacting.CompareAndSwap(false, true) {
		return nil, errServeSessionBusy
	}
	if rt.admittedActivity.Load() > 0 || rt.hasActiveSideQuestion() || !rt.mu.TryLock() {
		rt.compacting.Store(false)
		return nil, errServeSessionBusy
	}
	prepared := &preparedSessionCompaction{rt: rt, sessionID: sessionID}
	if rt.provider == nil || rt.store == nil || strings.TrimSpace(sessionID) == "" {
		prepared.release()
		return nil, errServeCompactionUnavailable
	}
	if !rt.ensurePersistedSession(ctx, sessionID, nil) || rt.sessionMeta == nil {
		prepared.release()
		return nil, errServeCompactionUnavailable
	}
	// A runtime rehydrated after restart restores provider resume state only
	// when a turn starts; native compaction needs it before any turn.
	rt.restoreProviderState(ctx, sessionID)
	systemPrompt := strings.TrimSpace(rt.systemPrompt)
	history := make([]core.Message, 0, len(rt.history))
	for _, message := range rt.history {
		if message.Role == core.RoleSystem {
			if systemPrompt == "" && rt.inputs.Load() == nil {
				systemPrompt = strings.TrimSpace(core.MessageText(message))
			}
			continue
		}
		history = append(history, message)
	}
	if len(history) < 2 {
		prepared.release()
		return nil, errServeCompactionTooSmall
	}

	model := strings.TrimSpace(rt.defaultModel)
	if model == "" && rt.sessionMeta != nil {
		model = strings.TrimSpace(rt.sessionMeta.Model)
	}
	config := engine.DefaultCompactionConfig()
	if rt.engine != nil && rt.engine.InputLimit() > 0 {
		config.InputLimit = rt.engine.InputLimit()
	} else if limit := llm.HelperInputLimitForProviderModel(rt.providerKey, model); limit > 0 {
		config.InputLimit = limit
	}

	prepared.history, prepared.systemPrompt = history, systemPrompt
	prepared.model, prepared.config = model, config
	return prepared, nil
}

func (p *preparedSessionCompaction) release() {
	p.rt.mu.Unlock()
	p.rt.compacting.Store(false)
	p.rt.Touch()
}

func (rt *serveRuntime) compactSession(ctx context.Context, sessionID string) (*engine.CompactionResult, error) {
	prepared, err := rt.prepareCompaction(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rt.beginCompactionActivity()
	result, err := prepared.execute(ctx)
	rt.finishCompactionActivityWithRelease(err, prepared.release)
	return result, err
}

func (p *preparedSessionCompaction) execute(ctx context.Context) (*engine.CompactionResult, error) {
	rt, sessionID := p.rt, p.sessionID
	// Providers that own their context (claude-bin) compact natively in place;
	// term-llm's transcript is left untouched.
	if native, ok, err := llm.NativeCompact(ctx, rt.provider, core.NativeCompactRequest{SystemPrompt: p.systemPrompt, Model: p.model}); ok {
		if err != nil {
			return nil, fmt.Errorf("compress conversation: %w", err)
		}
		rt.noteCompactionActivity(fmt.Sprintf("Context compacted natively (%d → %d tokens).", native.PreTokens, native.PostTokens))
		return nil, nil
	}
	result, err := llm.SoftCompact(ctx, rt.provider, p.model, p.systemPrompt, p.history, p.config)
	if err != nil {
		return nil, fmt.Errorf("compress conversation: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("compress conversation: empty result")
	}
	rt.recordHelperStats("compaction", result.Model, result.Usage)
	if rt.engine != nil {
		if err := rt.engine.PrepareCompactionContext(ctx, sessionID, rt.selectTools(nil), result); err != nil {
			slog.Warn("plan restoration after manual serve compaction failed; continuing without it", "error", err)
		}
	}

	updated, _, refreshed, err := session.ApplyCompaction(ctx, rt.store, rt.sessionMeta, nil, result)
	if err != nil {
		return nil, fmt.Errorf("save compressed conversation: %w", err)
	}
	if refreshed != nil {
		rt.sessionMeta = refreshed
	}
	compacted := make([]core.Message, 0, len(updated))
	for _, message := range updated {
		compacted = append(compacted, message.ToLLMMessage())
	}
	if len(compacted) == 0 {
		compacted = append(compacted, result.NewMessages...)
	}
	// rt.history is a snapshottable durable view. The next Engine.Stream call
	// reconstructs request-only plan context from session_plans.
	rt.history = compacted
	rt.historyPersisted = true
	rt.cumulativeUsage.Add(result.Usage)

	if !result.Usage.BillableCountersZero() && rt.sessionMeta != nil {
		if err := rt.store.UpdateMetrics(ctx, rt.sessionMeta.ID, 0, 0, result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.CachedInputTokens, result.Usage.CacheWriteTokens); err == nil {
			rt.sessionMeta.InputTokens += result.Usage.InputTokens
			rt.sessionMeta.OutputTokens += result.Usage.OutputTokens
			rt.sessionMeta.CachedInputTokens += result.Usage.CachedInputTokens
			rt.sessionMeta.CacheWriteTokens += result.Usage.CacheWriteTokens
			rt.recordDurableModelUsage(ctx, rt.sessionMeta.ID, result.Model, session.ModelUsageCompaction, result.Usage, 1, 0)
		}
	}
	if rt.engine != nil {
		rt.engine.SetContextEstimateBaseline(0, 0)
	}
	// Compaction rewrites the transcript, so any lane branched from the previous
	// one is gone and the live provider session no longer matches these messages.
	rt.invalidateSideQuestionSnapshot()
	rt.refreshSideQuestionSnapshot(rt.sideQuestionReplayBoundary(compacted))
	return result, nil
}
