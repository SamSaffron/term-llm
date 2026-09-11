package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/samsaffron/term-llm/internal/restart"
)

func (e *Engine) requestNativeToolFallback(req *Request, planner ToolSurfacePlanner, runID string, cause error, committed bool) (bool, string) {
	if req.NativeToolDiscovery == nil || cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || isContextOverflowError(cause) {
		return false, ""
	}
	nativePlanner, ok := planner.(NativeToolDiscoveryPlanner)
	if !ok {
		return false, ""
	}
	fallback, reason := nativePlanner.FallbackNativeToolDiscovery(runID, cause, committed)
	if fallback {
		resetProviderConversation(e.provider)
		slog.Warn("native tool discovery fell back to portable", "reason", reason, "session_id", req.SessionID)
	}
	return fallback, reason
}

func (e *Engine) openProviderTurnStream(ctx context.Context, req *Request, send eventSender, task *restart.Task, checkpointErr error, baseMessageCount, maxTurns int, planner ToolSurfacePlanner, runID string, modelSwitchOrdinal *int, originalToolChoice ToolChoice, originalTools *[]ToolSpec, compaction *runCompactionController, requestNativeFallback func(error, bool) (bool, string), attempt int, providerInFlight *bool) (Stream, error) {
	if task.Pending() && checkpointErr == nil {
		return nil, e.suspend(*req, attempt, nil, false, baseMessageCount)
	}
	// The final provider turn has no later agentic boundary. Reject arrivals
	// throughout that stream instead of accepting steering it cannot consume.
	e.beginSteeringRun(attempt < maxTurns-1)
	// A model-activated skill can tighten the filter between turns. Remove
	// now-disallowed ordinary definitions before the planner adds its narrowly
	// owned control surface for this exact run.
	req.Tools = e.FilterAllowedToolSpecs(req.Tools)
	if planner != nil {
		resetReason, err := planner.PrepareTurn(ctx, e.provider, req, runID, attempt, maxTurns)
		if err != nil {
			return nil, fmt.Errorf("prepare tool discovery turn: %w", err)
		}
		if resetReason != "" {
			resetProviderConversation(e.provider)
			slog.Debug("reset provider conversation for tool-surface change", "reason", resetReason, "session_id", req.SessionID, "turn", attempt)
		}
	}
	if len(req.Tools) == 0 && req.NativeToolDiscovery == nil {
		req.ToolChoice = ToolChoice{}
		req.LastTurnToolChoice = nil
	}

	// Inject any tool specs registered mid-loop (e.g. via skill activation).
	if pending := e.drainPendingToolSpecs(runID); len(pending) > 0 {
		pending = e.FilterAllowedToolSpecs(pending)
		for _, spec := range pending {
			if !hasToolNamed(req.Tools, spec.Name) {
				req.Tools = append(req.Tools, spec)
			}
			if !hasToolNamed((*originalTools), spec.Name) {
				(*originalTools) = append((*originalTools), spec)
			}
		}
	}

	if err := compaction.beforeTurn(); err != nil {
		return nil, err
	}
	// Warning when compaction is disabled but tracking detects high usage
	if compaction.config == nil && compaction.inputLimit > 0 && !e.contextNoticeEmitted.Load() && attempt > 0 {
		threshold := int(float64(compaction.inputLimit) * defaultThresholdRatio)
		est := e.estimatedTokens(req.Messages)
		if est >= threshold {
			e.contextNoticeEmitted.Store(true)
			pct := int(100 * float64(est) / float64(compaction.inputLimit))
			if err := send.Send(Event{Type: EventPhase, Text: fmt.Sprintf(WarningPhasePrefix+"context is %d%% full. Add auto_compact: true to your config to enable automatic compaction.", pct)}); err != nil {
				return nil, err
			}
		}
	}
	// Prepare turn
	if attempt == maxTurns-1 && attempt > 0 {
		req.Messages = append(req.Messages, SystemText(stopSearchToolHint))
		if req.LastTurnToolChoice != nil {
			req.ToolChoice = *req.LastTurnToolChoice
		}
	} else if attempt > 0 && !compaction.softActive {
		// Ensure we are in Auto mode for follow-up turns in the loop
		req.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}
	}

	if err := compaction.emitResumePhase(); err != nil {
		return nil, err
	}

	if err := e.applyPendingRequestModelSwitch(ctx, req, send, runID, modelSwitchOrdinal, attempt); err != nil {
		return nil, err
	}

	e.applyPendingServiceTier(req)
	providerReq := e.prepareProviderRequest(*req)

	// Log per-turn request state
	// For attempt 0: captures state after applyExternalSearch modifications
	// For attempt > 0: captures tool results appended in previous turn
	if e.debugLogger != nil {
		e.debugLogger.LogTurnRequest(attempt, e.provider.Name(), providerReq.Model, providerReq)
	}

	if req.DebugRaw {
		DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), providerReq, fmt.Sprintf("Request (turn %d)", attempt))
	}

	e.clearInlineFlush()
	if task.Pending() && checkpointErr == nil {
		return nil, e.suspend(*req, attempt, nil, false, baseMessageCount)
	}
	if err := e.awaitSteeringDispatch(ctx); err != nil {
		return nil, err
	}
	*providerInFlight = true
	stream, err := e.provider.Stream(ctx, providerReq)
	if err != nil {
		// Reactive compaction: if this is a context overflow error, try compacting and retrying (once)
		if compaction.config != nil && isContextOverflowError(err) && !compaction.reactiveDone {
			compaction.reactiveDone = true
			if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
				return nil, err
			}
			if compaction.softActive {
				if compaction.applySoftHardFallback((*originalTools), originalToolChoice) {
					return nil, errEngineTurnRetry
				}
			} else {
				result, compactErr := Compact(ctx, e.provider, req.Model, compaction.systemPrompt, nonSystemMessages(req.Messages), *compaction.config)
				if compactErr == nil && compaction.apply(result) {
					return nil, errEngineTurnRetry
				}
			}
		}
		if compaction.softActive {
			compaction.restoreSoftFailure((*originalTools), originalToolChoice)
		}
		// Warn when compaction is disabled and we hit context overflow
		if compaction.config == nil && compaction.inputLimit > 0 && !e.contextNoticeEmitted.Load() && isContextOverflowError(err) {
			e.contextNoticeEmitted.Store(true)
			if err := send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + "context overflow. Add auto_compact: true to your config to enable automatic compaction."}); err != nil {
				return nil, err
			}
		}
		if fallback, reason := requestNativeFallback(err, false); fallback {
			if sendErr := send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + reason}); sendErr != nil {
				return nil, sendErr
			}
			return nil, errEngineTurnRetry
		}
		return nil, err
	}

	return stream, nil
}
