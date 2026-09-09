package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

var (
	errServeCompactionUnavailable = errors.New("session compaction is unavailable")
	errServeCompactionTooSmall    = errors.New("not enough conversation history to compress")
)

func (rt *serveRuntime) compactSession(ctx context.Context, sessionID string) (*llm.CompactionResult, error) {
	if rt == nil {
		return nil, errServeCompactionUnavailable
	}
	if !rt.compacting.CompareAndSwap(false, true) {
		return nil, errServeSessionBusy
	}
	defer rt.compacting.Store(false)
	defer rt.Touch()

	if rt.hasActiveSideQuestion() || !rt.mu.TryLock() {
		return nil, errServeSessionBusy
	}
	defer rt.mu.Unlock()

	if rt.provider == nil || rt.store == nil || strings.TrimSpace(sessionID) == "" {
		return nil, errServeCompactionUnavailable
	}
	if !rt.ensurePersistedSession(ctx, sessionID, nil) || rt.sessionMeta == nil {
		return nil, errServeCompactionUnavailable
	}

	systemPrompt := strings.TrimSpace(rt.systemPrompt)
	history := make([]llm.Message, 0, len(rt.history))
	for _, message := range rt.history {
		if message.Role == llm.RoleSystem {
			if systemPrompt == "" && rt.inputs.Load() == nil {
				systemPrompt = strings.TrimSpace(llm.MessageText(message))
			}
			continue
		}
		history = append(history, message)
	}
	if len(history) < 2 {
		return nil, errServeCompactionTooSmall
	}

	model := strings.TrimSpace(rt.defaultModel)
	if model == "" && rt.sessionMeta != nil {
		model = strings.TrimSpace(rt.sessionMeta.Model)
	}
	config := llm.DefaultCompactionConfig()
	if rt.engine != nil && rt.engine.InputLimit() > 0 {
		config.InputLimit = rt.engine.InputLimit()
	} else if limit := llm.InputLimitForProviderModel(rt.providerKey, model); limit > 0 {
		config.InputLimit = limit
	}

	result, err := llm.SoftCompact(ctx, rt.provider, model, systemPrompt, history, config)
	if err != nil {
		return nil, fmt.Errorf("compress conversation: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("compress conversation: empty result")
	}
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
	compacted := make([]llm.Message, 0, len(updated))
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
		}
	}
	if rt.engine != nil {
		rt.engine.SetContextEstimateBaseline(0, 0)
	}
	rt.refreshSideQuestionSnapshot(compacted)
	return result, nil
}
