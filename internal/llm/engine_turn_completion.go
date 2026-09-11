package llm

import (
	"context"
	"strings"

	"github.com/samsaffron/term-llm/internal/restart"
)

// engineTurnCompletion borrows a settled provider attempt and the run-level
// values that its terminal path may advance. It never owns stream cleanup.
type engineTurnCompletion struct {
	ctx                                                               context.Context
	send                                                              eventSender
	req                                                               *Request
	attempt, maxTurns                                                 int
	runID                                                             string
	modelSwitchOrdinal, nextTurn                                      *int
	restoredToolChoice                                                *bool
	originalToolChoice                                                ToolChoice
	originalTools                                                     []ToolSpec
	turnCallback                                                      TurnCompletedCallback
	responseCallback                                                  ResponseCompletedCallback
	compaction                                                        *runCompactionController
	sendDone                                                          func() error
	task                                                              *restart.Task
	checkpointErr                                                     *error
	baseMessageCount                                                  int
	toolCalls, syncToolCalls                                          []ToolCall
	syncToolResults                                                   []Message
	textBuilder, reasoningBuilder                                     *strings.Builder
	reasoningSummaryParts                                             []string
	reasoningItemID, reasoningEncryptedContent                        string
	reasoningKind                                                     ReasoningKind
	providerReplayParts, inlineSyncParts                              []Part
	turnMetrics                                                       TurnMetrics
	buildOrderedInlineAssistant                                       func([]Part) Message
	finishingToolExecuted, preserveInlineToolOrder, syncToolsExecuted bool
	inlineToolLoop                                                    bool
	recoveredToolWork                                                 bool
	recoveryPriorErr, uncommittedPriorErr                             error
}

func (e *Engine) completeTextTurn(c *engineTurnCompletion) (bool, error) {
	ctx, send, req := c.ctx, c.send, *c.req
	attempt, maxTurns, runID := c.attempt, c.maxTurns, c.runID
	modelSwitchOrdinal, nextTurn := *c.modelSwitchOrdinal, *c.nextTurn
	restoredToolChoice := *c.restoredToolChoice
	originalToolChoice, originalTools := c.originalToolChoice, c.originalTools
	turnCallback := c.turnCallback
	compaction, sendDone := c.compaction, c.sendDone
	compaction.req = &req
	toolCalls := c.toolCalls
	textBuilder, reasoningBuilder := c.textBuilder, c.reasoningBuilder
	reasoningSummaryParts, reasoningItemID := c.reasoningSummaryParts, c.reasoningItemID
	reasoningEncryptedContent, reasoningKind := c.reasoningEncryptedContent, c.reasoningKind
	providerReplayParts := c.providerReplayParts
	turnMetrics := c.turnMetrics
	syncToolsExecuted := c.syncToolsExecuted
	recoveredToolWork := c.recoveredToolWork
	recoveryPriorErr, uncommittedPriorErr := c.recoveryPriorErr, c.uncommittedPriorErr
	defer func() {
		*c.req = req
		*c.modelSwitchOrdinal = modelSwitchOrdinal
		*c.nextTurn = nextTurn
		*c.restoredToolChoice = restoredToolChoice
		compaction.req = c.req
	}()
	if len(toolCalls) == 0 && !syncToolsExecuted {
		if recoveredToolWork && textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(reasoningSummaryParts) == 0 && reasoningItemID == "" && reasoningEncryptedContent == "" && len(providerReplayParts) == 0 && recoveryPriorErr != nil {
			return true, recoveryPriorErr
		}
		if uncommittedPriorErr != nil && textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(reasoningSummaryParts) == 0 && reasoningItemID == "" && reasoningEncryptedContent == "" && len(providerReplayParts) == 0 {
			return true, uncommittedPriorErr
		}
		// No tools called - check if we should restore original tool choice and retry once
		if originalToolChoice.Mode == ToolChoiceName && !restoredToolChoice && !compaction.softInjected {
			req.ToolChoice = originalToolChoice
			restoredToolChoice = true
			return true, errEngineTurnAdvance
		}
		// Call turnCallback with final text-only response (no tools)
		// Note: responseCallback is NOT called here because no tool execution follows.
		// responseCallback is only for persisting assistant messages before tool execution.
		var finalMsg Message
		if textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" || len(providerReplayParts) > 0 {
			finalMsg = buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(),
				nil,
				reasoningBuilder.String(),
				reasoningSummaryParts,
				reasoningItemID,
				reasoningEncryptedContent,
				reasoningKind,
			)
			finalMsg = attachProviderReplayParts(finalMsg, providerReplayParts)
			if compaction.softActive {
				brief := continuationBriefFromAssistantMessage(finalMsg)
				if brief != "" && compaction.config != nil && compaction.softOriginalCount > 0 {
					result := compactionResultFromBriefPrepared(compaction.systemPrompt, brief, compaction.softPrepared, compaction.softOriginalCount, *compaction.config)
					result.Model = strings.TrimSpace(req.Model)
					result.Usage = compaction.softUsage
					if compaction.apply(result) {
						compaction.resetSoft()
						req.Messages = append(req.Messages, UserText(contextContinuationPrompt))
						req.Tools = append([]ToolSpec(nil), originalTools...)
						req.ToolChoice = originalToolChoice
						return true, errEngineTurnRetry
					}
				} else if compaction.config != nil {
					if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
						return true, err
					}
					if compaction.applySoftHardFallback(originalTools, originalToolChoice) {
						return true, errEngineTurnRetry
					}
				}
				compaction.restoreSoftFailure(originalTools, originalToolChoice)
				return true, errEngineTurnRetry
			}
			if textBuilder.Len() == 0 && len(providerReplayParts) > 0 && attempt < maxTurns-1 {
				req.Messages = append(req.Messages, finalMsg)
				if turnCallback != nil {
					cbCtx, cancel := callbackContext(ctx)
					_ = turnCallback(cbCtx, attempt, []Message{finalMsg}, turnMetrics)
					cancel()
				}
				if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
					return true, err
				}
				return true, errEngineTurnAdvance
			}
			compaction.maybeAfterResponse([]Message{finalMsg})
			if turnCallback != nil {
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, []Message{finalMsg}, turnMetrics)
				cancel()
			}
		}
		if compaction.softActive {
			if compaction.config != nil {
				if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
					return true, err
				}
				if compaction.applySoftHardFallback(originalTools, originalToolChoice) {
					return true, errEngineTurnRetry
				}
			}
			compaction.restoreSoftFailure(originalTools, originalToolChoice)
			return true, errEngineTurnRetry
		}
		continued, err := e.continueWithSteering(ctx, send, &req, turnCallback, attempt, finalMsg, attempt < maxTurns-1, attempt < maxTurns-2)
		if err != nil {
			return true, err
		}
		if continued {
			if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
				return true, err
			}
			return true, errEngineTurnAdvance
		}
		if err := sendDone(); err != nil {
			return true, err
		}
		return true, nil
	}

	// If only sync tools were executed (MCP path), decide whether to continue
	return false, nil
}

func (e *Engine) completeSyncTurn(c *engineTurnCompletion) (bool, error) {
	ctx, send, req := c.ctx, c.send, *c.req
	attempt, maxTurns, runID := c.attempt, c.maxTurns, c.runID
	modelSwitchOrdinal, nextTurn := *c.modelSwitchOrdinal, *c.nextTurn
	restoredToolChoice := *c.restoredToolChoice
	turnCallback := c.turnCallback
	compaction, sendDone := c.compaction, c.sendDone
	compaction.req = &req
	toolCalls, syncToolCalls, syncToolResults := c.toolCalls, c.syncToolCalls, c.syncToolResults
	textBuilder, reasoningBuilder := c.textBuilder, c.reasoningBuilder
	reasoningSummaryParts, reasoningItemID := c.reasoningSummaryParts, c.reasoningItemID
	reasoningEncryptedContent, reasoningKind := c.reasoningEncryptedContent, c.reasoningKind
	providerReplayParts, inlineSyncParts := c.providerReplayParts, c.inlineSyncParts
	turnMetrics := c.turnMetrics
	buildOrderedInlineAssistant := c.buildOrderedInlineAssistant
	finishingToolExecuted := c.finishingToolExecuted
	preserveInlineToolOrder := c.preserveInlineToolOrder
	syncToolsExecuted, inlineToolLoop := c.syncToolsExecuted, c.inlineToolLoop
	defer func() {
		*c.req = req
		*c.modelSwitchOrdinal = modelSwitchOrdinal
		*c.nextTurn = nextTurn
		*c.restoredToolChoice = restoredToolChoice
		compaction.req = c.req
	}()

	if len(toolCalls) == 0 && syncToolsExecuted {
		// Preserve the provider's streamed text/tool/text ordering. Inline MCP loops
		// (Cursor, Grok, agy) can emit a final assistant segment after one or more
		// tools in the same stream; rebuilding from separate text/tool accumulators
		// moves that final text above the tools when the persisted message replaces
		// live output.
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

		// For MCP path, tools already executed synchronously during streaming,
		// so we call turnCallback with the complete turn (assistant + tool results).
		// ResponseCallback was effectively the streaming itself.
		if turnCallback != nil {
			turnMetrics.ToolCalls = len(syncToolCalls)
			turnMessages := []Message{assistantMsg}
			turnMessages = append(turnMessages, syncToolResults...)
			cbCtx, cancel := callbackContext(ctx)
			_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
			cancel()
		}

		// Inline-loop providers normally finish after their synchronous tool
		// stream. A dynamically registered tool is the exception: grok-bin
		// deliberately ends the current ACP prompt so the next provider turn can
		// reconnect with the expanded MCP catalogue. claude-bin and grok-bin
		// use the same tool-result flush for queued steering.
		if finishingToolExecuted {
			if err := sendDone(); err != nil {
				return true, err
			}
			return true, nil
		}
		if !inlineToolLoop {
			if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
				return true, err
			}
		}
		if inlineToolLoop {
			pendingTools := e.hasPendingToolSpecs(runID)
			canContinue := attempt < maxTurns-1
			canFlushSteering := pendingTools || e.providerSupportsInlineFlush()
			continued := false
			if pendingTools {
				req.Messages = append(req.Messages, UserText(dynamicToolContinuationPrompt))
			}
			if canFlushSteering && canContinue {
				var err error
				continued, err = e.continueWithSteering(ctx, send, &req, turnCallback, attempt, Message{}, true, attempt < maxTurns-2)
				if err != nil {
					return true, err
				}
			}
			if pendingTools || continued {
				if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
					return true, err
				}
				return true, errEngineTurnAdvance
			}
			if err := sendDone(); err != nil {
				return true, err
			}
			return true, nil
		}
		if attempt == maxTurns-1 {
			e.markSteeringRunNonConsuming()
			if err := send.Send(Event{Type: EventPhase, Text: MaxTurnsExceededWarning(maxTurns)}); err != nil {
				return true, err
			}
			return true, &MaxTurnsExceededError{MaxTurns: maxTurns}
		}

		// Check for user steering (MCP sync path)
		if steering := e.drainSteeringForNextTurn(attempt < maxTurns-2); len(steering) > 0 {
			steeringMsgs := make([]Message, 0, len(steering))
			for _, steering := range steering {
				steeringMsg := steering.Message
				steeringMsg.Role = RoleUser
				req.Messages = append(req.Messages, steeringMsg)
				steeringMsgs = append(steeringMsgs, steeringMsg)
			}
			if turnCallback != nil {
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, steeringMsgs, TurnMetrics{})
				cancel()
			}
			for _, steering := range steering {
				text := steering.DisplayText
				if text == "" {
					text = MessageText(steering.Message)
				}
				if err := send.Send(Event{Type: EventSteering, Text: text, SteeringID: steering.ID, Message: steering.Message, SteeringStatus: SteeringCommitted}); err != nil {
					return true, err
				}
			}
			if !inlineToolLoop {
				if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
					return true, err
				}
			}
		}

		// Continue the loop - provider will receive updated messages on next turn
		return true, errEngineTurnAdvance
	}
	return false, nil
}

func (e *Engine) completeAsyncTurn(c *engineTurnCompletion) error {
	ctx, send, req := c.ctx, c.send, *c.req
	attempt, maxTurns, runID := c.attempt, c.maxTurns, c.runID
	modelSwitchOrdinal, nextTurn := *c.modelSwitchOrdinal, *c.nextTurn
	restoredToolChoice := *c.restoredToolChoice
	turnCallback, responseCallback := c.turnCallback, c.responseCallback
	compaction, sendDone := c.compaction, c.sendDone
	compaction.req = &req
	task, checkpointErr, baseMessageCount := c.task, c.checkpointErr, c.baseMessageCount
	toolCalls := c.toolCalls
	textBuilder, reasoningBuilder := c.textBuilder, c.reasoningBuilder
	reasoningSummaryParts, reasoningItemID := c.reasoningSummaryParts, c.reasoningItemID
	reasoningEncryptedContent, reasoningKind := c.reasoningEncryptedContent, c.reasoningKind
	providerReplayParts := c.providerReplayParts
	turnMetrics := c.turnMetrics
	finishingToolExecuted := c.finishingToolExecuted
	defer func() {
		*c.req = req
		*c.modelSwitchOrdinal = modelSwitchOrdinal
		*c.nextTurn = nextTurn
		*c.restoredToolChoice = restoredToolChoice
		compaction.req = c.req
	}()

	toolCalls = ensureToolCallIDs(toolCalls)
	toolCalls = dedupeToolCalls(toolCalls)

	// Split into registered (to execute) and unregistered (to passthrough).
	// ToolMap allows client tool names to be redirected to server tools
	// (e.g. "WebSearch" → "search"). The call keeps its original name
	// so the client sees the name it expects in the response.
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

	// Debug log unregistered tool calls (already forwarded during streaming)
	for i := range unregistered {
		DebugToolCall(req.Debug, unregistered[i])
	}

	// If nothing to execute, we are done
	if len(registered) == 0 {
		// Call turnCallback with text + unregistered tool calls
		// Note: responseCallback is NOT called here because no tool execution follows.
		// responseCallback is only for persisting assistant messages before tool execution.
		unregisteredWithInfo := e.withToolPreview(unregistered)
		finalMsg := buildAssistantMessageWithReasoningMetadata(
			textBuilder.String(),
			unregisteredWithInfo,
			reasoningBuilder.String(),
			reasoningSummaryParts,
			reasoningItemID,
			reasoningEncryptedContent,
			reasoningKind,
		)
		finalMsg = attachProviderReplayParts(finalMsg, providerReplayParts)
		if len(finalMsg.Parts) > 0 {
			compaction.maybeAfterResponse([]Message{finalMsg})
			if turnCallback != nil {
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, []Message{finalMsg}, turnMetrics)
				cancel()
			}
		}
		continued, err := e.continueWithSteering(ctx, send, &req, turnCallback, attempt, finalMsg, attempt < maxTurns-1, attempt < maxTurns-2)
		if err != nil {
			return err
		}
		if continued {
			if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
				return err
			}
			return errEngineTurnAdvance
		}
		if err := sendDone(); err != nil {
			return err
		}
		return nil
	}

	if attempt == maxTurns-1 {
		e.markSteeringRunNonConsuming()
		if err := send.Send(Event{Type: EventPhase, Text: MaxTurnsExceededWarning(maxTurns)}); err != nil {
			return err
		}
		return &MaxTurnsExceededError{MaxTurns: maxTurns}
	}

	// Build assistant message with text + tool calls + reasoning
	// (built before tool execution so we can save it incrementally)
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

	// Call responseCallback BEFORE tool execution to persist assistant message
	// This ensures the message is saved even if tool execution fails/crashes
	responseHandled := callResponseCompletedCallback(ctx, responseCallback, attempt, assistantMsg, turnMetrics)
	if task.Pending() && *checkpointErr == nil && (responseHandled || responseCallback == nil) {
		req.Messages = append(req.Messages, assistantMsg)
		return e.suspend(req, attempt, registered, false, baseMessageCount, turnMetrics)
	}

	// ToolMap: swap client tool names to mapped server names for execution.
	// We save original names keyed by call ID so we can restore them on
	// the registered slice and the tool-result messages afterwards.
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

	// Execute registered tools
	for _, call := range registered {
		DebugToolCall(req.Debug, call)
		info := e.getToolPreview(call)

		if err := send.Send(Event{Type: EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolArgs: call.Arguments}); err != nil {
			return err
		}
	}

	transcriptForApproval := buildApprovalTranscript(req.ApprovalTranscriptPrefix, req.Messages, assistantMsg)
	toolResults, err := e.executeToolCalls(ctx, registered, req.ParallelToolCalls, send, req.Debug, req.DebugRaw, transcriptForApproval)
	if err != nil {
		return err
	}

	finishingToolExecuted = false
	for _, call := range registered {
		if e.tools.IsFinishingTool(call.Name) {
			finishingToolExecuted = true
			break
		}
	}

	// Restore original (client-facing) names so conversation history
	// references the names the client expects.
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
	nextTurn = attempt + 1

	// Call turn completed callback with tool results for incremental persistence
	if turnCallback != nil {
		turnMetrics.ToolCalls = len(registered)
		turnMessages := turnMessagesAfterResponseCallback(responseHandled, assistantMsg, toolResults)
		cbCtx, cancel := callbackContext(ctx)
		_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
		cancel()
	}

	// Exit promptly if caller cancelled while tools were executing.
	// Check after the turn callback so in-progress tool results are persisted
	// before we abandon the loop.
	if err := ctx.Err(); err != nil {
		return err
	}

	if finishingToolExecuted {
		if err := sendDone(); err != nil {
			return err
		}
		return nil
	}
	if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
		return err
	}

	// Check for user steering queued during this turn.
	// If present, inject them as FIFO user messages so the LLM sees them on the next turn.
	if steering := e.drainSteeringForNextTurn(attempt < maxTurns-2); len(steering) > 0 {
		steeringMsgs := make([]Message, 0, len(steering))
		for _, steering := range steering {
			steeringMsg := steering.Message
			steeringMsg.Role = RoleUser
			req.Messages = append(req.Messages, steeringMsg)
			steeringMsgs = append(steeringMsgs, steeringMsg)
		}
		// Fire turn callback so steering are persisted
		if turnCallback != nil {
			cbCtx, cancel := callbackContext(ctx)
			_ = turnCallback(cbCtx, attempt, steeringMsgs, TurnMetrics{})
			cancel()
		}
		// Emit events so UIs can display committed steering inline
		for _, steering := range steering {
			text := steering.DisplayText
			if text == "" {
				text = MessageText(steering.Message)
			}
			if err := send.Send(Event{Type: EventSteering, Text: text, SteeringID: steering.ID, Message: steering.Message, SteeringStatus: SteeringCommitted}); err != nil {
				return err
			}
		}
		if err := e.applyPendingRequestModelSwitch(ctx, &req, send, runID, &modelSwitchOrdinal, attempt+1); err != nil {
			return err
		}
	}
	return errEngineTurnAdvance
}
