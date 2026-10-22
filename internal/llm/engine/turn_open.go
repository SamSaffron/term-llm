package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"

	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func (e *Engine) requestNativeToolFallback(req *core.Request, planner core.ToolSurfacePlanner, runID string, cause error, committed bool) (bool, string) {
	if req.NativeToolDiscovery == nil || cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || IsContextOverflowError(cause) {
		return false, ""
	}
	nativePlanner, ok := planner.(core.NativeToolDiscoveryPlanner)
	if !ok {
		return false, ""
	}
	fallback, reason := nativePlanner.FallbackNativeToolDiscovery(runID, cause, committed)
	if fallback {
		resetProviderConversation(e.provider)
		runtimeoutput.Warn("native tool discovery fell back to portable", "reason", reason, "session_id", req.SessionID)
	}
	return fallback, reason
}

func (e *Engine) openProviderTurnStream(ctx context.Context, req *core.Request, send corestream.Sender, task *restart.Task, checkpointErr error, baseMessageCount, maxTurns int, planner core.ToolSurfacePlanner, runID string, modelSwitchOrdinal *int, originalToolChoice core.ToolChoice, forcedChoice *forcedChoiceState, originalTools *[]core.ToolSpec, compaction *runCompactionController, requestNativeFallback func(error, bool) (bool, string), attempt int, providerInFlight *bool) (core.Stream, error) {
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
	// Choose this follow-up turn's tool_choice before the planner prepares it:
	// the planner validates forced tool_search against its discovery cutoff, so it
	// must not see a force that a previous turn already satisfied. The final turn
	// is handled below (LastTurnToolChoice); an active soft compaction owns it.
	if attempt > 0 && attempt != maxTurns-1 && !compaction.softActive {
		if *forcedChoice == forcedChoiceRetryNext {
			// The previous turn ignored a forced tool_choice; retry it forced.
			// The state is spent when this turn completes, so an uncommitted
			// retry of the same turn stays forced.
			req.ToolChoice = originalToolChoice
		} else {
			// Ensure we are in Auto mode for follow-up turns in the loop
			req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
		}
	}
	if planner != nil {
		resetReason, err := planner.PrepareTurn(ctx, e.provider, req, runID, attempt, maxTurns)
		if err != nil {
			return nil, fmt.Errorf("prepare tool discovery turn: %w", err)
		}
		if resetReason != "" {
			resetProviderConversation(e.provider)
			runtimeoutput.Debug("reset provider conversation for tool-surface change", "reason", resetReason, "session_id", req.SessionID, "turn", attempt)
		}
	}
	if len(req.Tools) == 0 && req.NativeToolDiscovery == nil {
		req.ToolChoice = core.ToolChoice{}
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
		threshold := int(float64(compaction.inputLimit) * DefaultThresholdRatio)
		est := e.estimatedTokens(req.Messages)
		if est >= threshold {
			e.contextNoticeEmitted.Store(true)
			pct := int(100 * float64(est) / float64(compaction.inputLimit))
			if err := send.Send(core.Event{Type: core.EventPhase, Text: fmt.Sprintf(core.WarningPhasePrefix+"context is %d%% full. Add auto_compact: true to your config to enable automatic compaction.", pct)}); err != nil {
				return nil, err
			}
		}
	}
	// Prepare the final turn (follow-up turns were prepared before the planner).
	if attempt == maxTurns-1 && attempt > 0 {
		req.Messages = append(req.Messages, core.SystemText(stopSearchToolHint))
		if req.LastTurnToolChoice != nil {
			req.ToolChoice = *req.LastTurnToolChoice
		}
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
		env.debugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), providerReq, fmt.Sprintf("Request (turn %d)", attempt))
	}

	e.ClearInlineFlush()
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
		if compaction.config != nil && IsContextOverflowError(err) && !compaction.reactiveDone {
			if retry, reactErr := compaction.compactReactiveOverflow((*originalTools), originalToolChoice); reactErr != nil {
				return nil, reactErr
			} else if retry {
				return nil, errEngineTurnRetry
			}
		}
		if compaction.softActive {
			compaction.restoreSoftFailure((*originalTools), originalToolChoice, err)
		}
		// Warn when compaction is disabled and we hit context overflow
		if compaction.config == nil && compaction.inputLimit > 0 && !e.contextNoticeEmitted.Load() && IsContextOverflowError(err) {
			e.contextNoticeEmitted.Store(true)
			if err := send.Send(core.Event{Type: core.EventPhase, Text: core.WarningPhasePrefix + "context overflow. Add auto_compact: true to your config to enable automatic compaction."}); err != nil {
				return nil, err
			}
		}
		if fallback, reason := requestNativeFallback(err, false); fallback {
			if sendErr := send.Send(core.Event{Type: core.EventPhase, Text: core.WarningPhasePrefix + reason}); sendErr != nil {
				return nil, sendErr
			}
			return nil, errEngineTurnRetry
		}
		return nil, err
	}

	return stream, nil
}
