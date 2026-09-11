package llm

import (
	"context"
	"log/slog"
	"strings"
)

type engineTurnRecovery struct {
	ctx                                        context.Context
	req                                        *Request
	send                                       eventSender
	attempt                                    int
	settleSyncTools                            func()
	toolCalls                                  *[]ToolCall
	syncToolsExecuted                          *bool
	recoveredToolCallIDs                       *map[string]bool
	recoveredToolWork                          *bool
	recoveredAtMessageCount                    *int
	recoveryPriorErr                           *error
	syncToolCalls                              *[]ToolCall
	syncToolResults                            *[]Message
	preserveInlineToolOrder                    bool
	inlineSyncParts                            *[]Part
	buildOrderedInlineAssistant                func([]Part) Message
	textBuilder, reasoningBuilder              *strings.Builder
	reasoningSummaryParts                      *[]string
	reasoningItemID, reasoningEncryptedContent *string
	reasoningKind                              *ReasoningKind
	providerReplayParts                        *[]Part
	compaction                                 *runCompactionController
	turnCallback                               TurnCompletedCallback
	turnMetrics                                *TurnMetrics
	runID                                      string
	modelSwitchOrdinal                         *int
	responseCallback                           ResponseCompletedCallback
	finishingToolExecuted                      *bool
	recoveryCompleted                          *bool
	sendDone                                   func() error
}

func (e *Engine) recoverCommittedTurn(r *engineTurnRecovery, cause error) (bool, error) {
	if !isCommittedStreamRecoveryError(cause) {
		return false, nil
	}
	// Snapshot callback-produced sync outcomes only after settlement. Failure
	// handlers normally settle first; keeping this order makes recovery robust if
	// supervisor settlement becomes non-idempotent in the future.
	r.settleSyncTools()

	ctx, req, send, attempt := r.ctx, *r.req, r.send, r.attempt
	toolCalls, syncToolsExecuted := *r.toolCalls, *r.syncToolsExecuted
	recoveredToolCallIDs, recoveredToolWork := *r.recoveredToolCallIDs, *r.recoveredToolWork
	recoveredAtMessageCount, recoveryPriorErr := *r.recoveredAtMessageCount, *r.recoveryPriorErr
	syncToolCalls, syncToolResults := *r.syncToolCalls, *r.syncToolResults
	preserveInlineToolOrder, inlineSyncParts := r.preserveInlineToolOrder, *r.inlineSyncParts
	buildOrderedInlineAssistant := r.buildOrderedInlineAssistant
	textBuilder, reasoningBuilder := r.textBuilder, r.reasoningBuilder
	reasoningSummaryParts, reasoningItemID := *r.reasoningSummaryParts, *r.reasoningItemID
	reasoningEncryptedContent, reasoningKind := *r.reasoningEncryptedContent, *r.reasoningKind
	providerReplayParts, compaction := *r.providerReplayParts, r.compaction
	compaction.req = &req
	turnCallback, turnMetrics := r.turnCallback, *r.turnMetrics
	runID, modelSwitchOrdinal := r.runID, *r.modelSwitchOrdinal
	responseCallback, finishingToolExecuted, sendDone := r.responseCallback, *r.finishingToolExecuted, r.sendDone
	recoveryCompleted := *r.recoveryCompleted
	defer func() {
		*r.req = req
		*r.toolCalls = toolCalls
		*r.recoveredToolCallIDs = recoveredToolCallIDs
		*r.recoveredToolWork = recoveredToolWork
		*r.recoveredAtMessageCount = recoveredAtMessageCount
		*r.recoveryPriorErr = recoveryPriorErr
		*r.turnMetrics = turnMetrics
		*r.modelSwitchOrdinal = modelSwitchOrdinal
		*r.finishingToolExecuted = finishingToolExecuted
		*r.recoveryCompleted = recoveryCompleted
		compaction.req = r.req
	}()

	if len(toolCalls) == 0 && !syncToolsExecuted {
		return false, nil
	}

	if len(toolCalls) > 0 {
		candidate := ensureToolCallIDs(dedupeToolCalls(toolCalls))
		allRecovered := len(candidate) > 0 && recoveredToolCallIDs != nil
		for _, call := range candidate {
			if !recoveredToolCallIDs[call.ID] {
				allRecovered = false
				break
			}
		}
		if allRecovered {
			return false, nil
		}
	}

	if err := send.Send(Event{Type: EventRetry, RetryAttempt: 1, RetryMaxAttempts: 1, RetryWaitSecs: 0}); err != nil {
		return false, err
	}
	if cause != nil {
		slog.Debug("recovering stream failure from journaled tool work", "error", cause)
	}
	recoveredToolWork = true
	recoveredAtMessageCount = len(req.Messages)
	recoveryPriorErr = cause

	if len(toolCalls) == 0 && syncToolsExecuted {
		var assistantMsg Message
		if preserveInlineToolOrder && len(inlineSyncParts) > 0 {
			assistantMsg = buildOrderedInlineAssistant(inlineSyncParts)
		} else {
			assistantMsg = buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(),
				e.withToolPreview(syncToolCalls),
				reasoningBuilder.String(),
				reasoningSummaryParts,
				reasoningItemID,
				reasoningEncryptedContent,
				reasoningKind,
			)
		}
		assistantMsg = attachProviderReplayParts(assistantMsg, providerReplayParts)
		compaction.maybeAfterResponse(append([]Message{assistantMsg}, syncToolResults...))
		req.Messages = append(req.Messages, assistantMsg)
		req.Messages = append(req.Messages, syncToolResults...)
		recoveredAtMessageCount = len(req.Messages)
		if turnCallback != nil {
			turnMetrics.ToolCalls = len(syncToolCalls)
			turnMessages := []Message{assistantMsg}
			turnMessages = append(turnMessages, syncToolResults...)
			cbCtx, cancel := callbackContext(ctx)
			_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
			cancel()
		}
		if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
			return false, err
		}
		return true, nil
	}

	toolCalls = ensureToolCallIDs(toolCalls)
	toolCalls = dedupeToolCalls(toolCalls)
	if recoveredToolCallIDs == nil {
		recoveredToolCallIDs = make(map[string]bool)
	}
	for _, call := range toolCalls {
		recoveredToolCallIDs[call.ID] = true
	}

	var registered, unregistered []ToolCall
	for _, call := range toolCalls {
		lookupName := call.Name
		if req.ToolMap != nil {
			if mapped, ok := req.ToolMap[call.Name]; ok {
				lookupName = mapped
			}
		}
		if _, ok := e.tools.Get(lookupName); ok {
			registered = append(registered, call)
		} else {
			unregistered = append(unregistered, call)
		}
	}

	if len(registered) == 0 {
		// Nothing local can be executed, but the assistant tool-call item is still
		// a completed model item. Journal it so a caller/session can resume with
		// the exact model-visible state that existed at the disconnect.
		unregisteredWithInfo := e.withToolPreview(unregistered)
		assistantMsg := buildAssistantMessageWithReasoningMetadata(
			textBuilder.String(),
			unregisteredWithInfo,
			reasoningBuilder.String(),
			reasoningSummaryParts,
			reasoningItemID,
			reasoningEncryptedContent,
			reasoningKind,
		)
		assistantMsg = attachProviderReplayParts(assistantMsg, providerReplayParts)
		if len(assistantMsg.Parts) > 0 {
			compaction.maybeAfterResponse([]Message{assistantMsg})
			req.Messages = append(req.Messages, assistantMsg)
			recoveredAtMessageCount = len(req.Messages)
			if turnCallback != nil {
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, []Message{assistantMsg}, turnMetrics)
				cancel()
			}
		}
		return false, cause
	}

	assistantMsg := buildAssistantMessageWithReasoningMetadata(
		textBuilder.String(),
		e.withToolPreview(registered),
		reasoningBuilder.String(),
		reasoningSummaryParts,
		reasoningItemID,
		reasoningEncryptedContent,
		reasoningKind,
	)
	assistantMsg = attachProviderReplayParts(assistantMsg, providerReplayParts)
	compaction.maybeAfterResponse([]Message{assistantMsg})
	responseHandled := callResponseCompletedCallback(ctx, responseCallback, attempt, assistantMsg, turnMetrics)

	var origNameByID map[string]string
	if req.ToolMap != nil {
		for i := range registered {
			if mapped, ok := req.ToolMap[registered[i].Name]; ok {
				if origNameByID == nil {
					origNameByID = make(map[string]string)
				}
				origNameByID[registered[i].ID] = registered[i].Name
				registered[i].Name = mapped
			}
		}
	}

	for _, call := range registered {
		DebugToolCall(req.Debug, call)
		info := e.getToolPreview(call)
		if err := send.Send(Event{Type: EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolArgs: call.Arguments}); err != nil {
			return false, err
		}
	}

	transcriptForApproval := buildApprovalTranscript(req.ApprovalTranscriptPrefix, req.Messages, assistantMsg)
	toolResults, err := e.executeToolCalls(ctx, registered, req.ParallelToolCalls, send, req.Debug, req.DebugRaw, transcriptForApproval)
	if err != nil {
		return false, err
	}

	finishingToolExecuted = false
	for _, call := range registered {
		if e.tools.IsFinishingTool(call.Name) {
			finishingToolExecuted = true
			break
		}
	}

	if origNameByID != nil {
		for i := range registered {
			if orig, ok := origNameByID[registered[i].ID]; ok {
				registered[i].Name = orig
			}
		}
		for i := range toolResults {
			for j := range toolResults[i].Parts {
				if toolResults[i].Parts[j].ToolResult != nil {
					if orig, ok := origNameByID[toolResults[i].Parts[j].ToolResult.ID]; ok {
						toolResults[i].Parts[j].ToolResult.Name = orig
					}
				}
			}
		}
	}

	req.Messages = append(req.Messages, assistantMsg)
	req.Messages = append(req.Messages, toolResults...)
	recoveredAtMessageCount = len(req.Messages)
	if turnCallback != nil {
		turnMetrics.ToolCalls = len(registered)
		turnMessages := turnMessagesAfterResponseCallback(responseHandled, assistantMsg, toolResults)
		cbCtx, cancel := callbackContext(ctx)
		_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if finishingToolExecuted {
		if err := sendDone(); err != nil {
			return false, err
		}
		recoveryCompleted = true
		return true, nil
	}
	if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
		return false, err
	}
	return true, nil
}
