package llm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
)

type engineUncommittedRetry struct {
	send                           eventSender
	compaction                     *runCompactionController
	recoveredToolWork              *bool
	toolCalls                      *[]ToolCall
	syncToolsExecuted              *bool
	scratchpadCommitted            *bool
	scratchpadHasDiscardableOutput *bool
	retries                        *int
	priorErr                       *error
}

func (r *engineUncommittedRetry) retry(cause error) (bool, error) {
	if cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || !isUncommittedReplayableStreamError(cause) {
		return false, nil
	}
	if *r.recoveredToolWork || len(*r.toolCalls) > 0 || *r.syncToolsExecuted || *r.scratchpadCommitted {
		return false, nil
	}
	if *r.retries >= defaultUncommittedStreamMaxRetries {
		return false, nil
	}
	*r.retries++
	*r.priorErr = cause
	if *r.scratchpadHasDiscardableOutput {
		if err := r.send.Send(Event{Type: EventAttemptDiscard}); err != nil {
			return false, err
		}
	}
	if err := r.send.Send(Event{Type: EventRetry, RetryAttempt: *r.retries, RetryMaxAttempts: defaultUncommittedStreamMaxRetries, RetryWaitSecs: 0}); err != nil {
		return false, err
	}
	slog.Debug("retrying failed uncommitted model stream", "attempt", *r.retries, "error", cause)
	*r.scratchpadHasDiscardableOutput = false
	if r.compaction.softActive {
		r.compaction.softUsage = Usage{}
	}
	return true, nil
}

type engineStreamFailureContext struct {
	ctx                                                                    context.Context
	req                                                                    *Request
	send                                                                   eventSender
	stream                                                                 Stream
	compaction                                                             *runCompactionController
	originalTools                                                          []ToolSpec
	originalToolChoice                                                     ToolChoice
	recoveredToolWork                                                      *bool
	recoveredAtMessageCount                                                *int
	toolCalls, syncToolCalls                                               *[]ToolCall
	textBuilder, reasoningBuilder                                          *strings.Builder
	syncToolsExecuted, scratchpadCommitted, scratchpadHasDiscardableOutput *bool
	finishSyncToolsAfterStreamFailure                                      func(error)
	planner                                                                ToolSurfacePlanner
	runID                                                                  string
	retry                                                                  engineUncommittedRetry
	recovery                                                               engineTurnRecovery
	recoveryCompleted                                                      *bool
}

func (e *Engine) handleChaosStreamFailure(f *engineStreamFailureContext, chaosErr error) error {
	req, send := f.req, f.send
	recoveredToolWork, recoveredAtMessageCount := *f.recoveredToolWork, *f.recoveredAtMessageCount
	toolCalls := *f.toolCalls
	syncToolsExecuted := *f.syncToolsExecuted
	scratchpadCommitted, scratchpadHasDiscardableOutput := *f.scratchpadCommitted, *f.scratchpadHasDiscardableOutput
	finishSyncToolsAfterStreamFailure := f.finishSyncToolsAfterStreamFailure
	requestNativeFallback := func(cause error, committed bool) (bool, string) {
		return e.requestNativeToolFallback(f.req, f.planner, f.runID, cause, committed)
	}
	retryUncommittedAttempt := f.retry.retry
	recoverCommittedToolWork := func(cause error) (bool, error) {
		return e.recoverCommittedTurn(&f.recovery, cause)
	}
	stream := f.stream
	finishSyncToolsAfterStreamFailure(chaosErr)
	stream.Close()
	if fallback, reason := requestNativeFallback(chaosErr, scratchpadCommitted || len(toolCalls) > 0 || syncToolsExecuted); fallback {
		if scratchpadHasDiscardableOutput {
			if sendErr := send.Send(Event{Type: EventAttemptDiscard}); sendErr != nil {
				return sendErr
			}
		}
		if sendErr := send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + reason}); sendErr != nil {
			return sendErr
		}
		return errEngineTurnRetry
	}
	if recoveredToolWork && len(req.Messages) == recoveredAtMessageCount && len(toolCalls) == 0 && !syncToolsExecuted {
		return chaosErr
	}
	if retried, retryErr := retryUncommittedAttempt(chaosErr); retryErr != nil {
		return retryErr
	} else if retried {
		return errEngineTurnRetry
	}
	if recovered, recoverErr := recoverCommittedToolWork(chaosErr); recoverErr != nil {
		return recoverErr
	} else if *f.recoveryCompleted {
		return nil
	} else if recovered {
		return errEngineTurnAdvance
	}
	return chaosErr
}

func (e *Engine) handleReceiveStreamFailure(f *engineStreamFailureContext, err error) error {
	req, send := f.req, f.send
	compaction := f.compaction
	originalTools, originalToolChoice := f.originalTools, f.originalToolChoice
	recoveredToolWork, recoveredAtMessageCount := *f.recoveredToolWork, *f.recoveredAtMessageCount
	toolCalls, syncToolCalls := *f.toolCalls, *f.syncToolCalls
	textBuilder, reasoningBuilder := f.textBuilder, f.reasoningBuilder
	syncToolsExecuted := *f.syncToolsExecuted
	scratchpadCommitted, scratchpadHasDiscardableOutput := *f.scratchpadCommitted, *f.scratchpadHasDiscardableOutput
	finishSyncToolsAfterStreamFailure := f.finishSyncToolsAfterStreamFailure
	requestNativeFallback := func(cause error, committed bool) (bool, string) {
		return e.requestNativeToolFallback(f.req, f.planner, f.runID, cause, committed)
	}
	retryUncommittedAttempt := f.retry.retry
	recoverCommittedToolWork := func(cause error) (bool, error) {
		return e.recoverCommittedTurn(&f.recovery, cause)
	}
	stream, ctx := f.stream, f.ctx
	finishSyncToolsAfterStreamFailure(err)
	stream.Close()
	if compaction.config != nil && isContextOverflowError(err) && !compaction.reactiveDone && textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(toolCalls) == 0 && len(syncToolCalls) == 0 {
		compaction.reactiveDone = true
		if sendErr := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); sendErr != nil {
			return sendErr
		}
		if compaction.softActive {
			if compaction.applySoftHardFallback(originalTools, originalToolChoice) {
				return errEngineTurnRetry
			}
		} else {
			result, compactErr := Compact(ctx, e.provider, req.Model, compaction.systemPrompt, nonSystemMessages(req.Messages), *compaction.config)
			if compactErr == nil && compaction.apply(result) {
				return errEngineTurnRetry
			}
		}
	}
	if recoveredToolWork && len(req.Messages) == recoveredAtMessageCount && len(toolCalls) == 0 && !syncToolsExecuted {
		return err
	}
	if fallback, reason := requestNativeFallback(err, scratchpadCommitted || len(toolCalls) > 0 || syncToolsExecuted); fallback {
		if scratchpadHasDiscardableOutput {
			if sendErr := send.Send(Event{Type: EventAttemptDiscard}); sendErr != nil {
				return sendErr
			}
		}
		if sendErr := send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + reason}); sendErr != nil {
			return sendErr
		}
		return errEngineTurnRetry
	}
	if retried, retryErr := retryUncommittedAttempt(err); retryErr != nil {
		return retryErr
	} else if retried {
		return errEngineTurnRetry
	}
	if recovered, recoverErr := recoverCommittedToolWork(err); recoverErr != nil {
		return recoverErr
	} else if *f.recoveryCompleted {
		return nil
	} else if recovered {
		return errEngineTurnAdvance
	}
	if compaction.softActive {
		compaction.restoreSoftFailure(originalTools, originalToolChoice)
	}
	return err
}

func (e *Engine) handleEventStreamFailure(f *engineStreamFailureContext, eventErr error) error {
	req, send := f.req, f.send
	compaction := f.compaction
	originalTools, originalToolChoice := f.originalTools, f.originalToolChoice
	recoveredToolWork, recoveredAtMessageCount := *f.recoveredToolWork, *f.recoveredAtMessageCount
	toolCalls, syncToolCalls := *f.toolCalls, *f.syncToolCalls
	textBuilder, reasoningBuilder := f.textBuilder, f.reasoningBuilder
	syncToolsExecuted := *f.syncToolsExecuted
	scratchpadCommitted, scratchpadHasDiscardableOutput := *f.scratchpadCommitted, *f.scratchpadHasDiscardableOutput
	finishSyncToolsAfterStreamFailure := f.finishSyncToolsAfterStreamFailure
	requestNativeFallback := func(cause error, committed bool) (bool, string) {
		return e.requestNativeToolFallback(f.req, f.planner, f.runID, cause, committed)
	}
	retryUncommittedAttempt := f.retry.retry
	recoverCommittedToolWork := func(cause error) (bool, error) {
		return e.recoverCommittedTurn(&f.recovery, cause)
	}
	stream, ctx := f.stream, f.ctx
	finishSyncToolsAfterStreamFailure(eventErr)
	stream.Close()
	if compaction.config != nil && isContextOverflowError(eventErr) && !compaction.reactiveDone && textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(toolCalls) == 0 && len(syncToolCalls) == 0 {
		compaction.reactiveDone = true
		if sendErr := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); sendErr != nil {
			return sendErr
		}
		if compaction.softActive {
			if compaction.applySoftHardFallback(originalTools, originalToolChoice) {
				return errEngineTurnRetry
			}
		} else {
			result, compactErr := Compact(ctx, e.provider, req.Model, compaction.systemPrompt, nonSystemMessages(req.Messages), *compaction.config)
			if compactErr == nil && compaction.apply(result) {
				return errEngineTurnRetry
			}
		}
	}
	if recoveredToolWork && len(req.Messages) == recoveredAtMessageCount && len(toolCalls) == 0 && !syncToolsExecuted {
		return eventErr
	}
	if fallback, reason := requestNativeFallback(eventErr, scratchpadCommitted || len(toolCalls) > 0 || syncToolsExecuted); fallback {
		if scratchpadHasDiscardableOutput {
			if sendErr := send.Send(Event{Type: EventAttemptDiscard}); sendErr != nil {
				return sendErr
			}
		}
		if sendErr := send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + reason}); sendErr != nil {
			return sendErr
		}
		return errEngineTurnRetry
	}
	if retried, retryErr := retryUncommittedAttempt(eventErr); retryErr != nil {
		return retryErr
	} else if retried {
		return errEngineTurnRetry
	}
	if recovered, recoverErr := recoverCommittedToolWork(eventErr); recoverErr != nil {
		return recoverErr
	} else if *f.recoveryCompleted {
		return nil
	} else if recovered {
		return errEngineTurnAdvance
	}
	if compaction.softActive {
		compaction.restoreSoftFailure(originalTools, originalToolChoice)
	}
	return eventErr
}
