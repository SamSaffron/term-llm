package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/restart"
)

var (
	errEngineTurnAdvance = errors.New("advance engine turn")
	errEngineTurnRetry   = errors.New("retry engine turn")
)

// providerTurnRunFrame owns values that survive helper calls during one
// provider turn. It is copied back to the run-loop owner before returning.
type providerTurnRunFrame struct {
	req                      Request
	restoredToolChoice       bool
	recoveredToolWork        bool
	recoveredToolCallIDs     map[string]bool
	recoveredAtMessageCount  int
	recoveryPriorErr         error
	uncommittedStreamRetries int
	uncommittedPriorErr      error
	activeSyncTools          *syncToolSupervisor
	nextTurn                 int
	providerInFlight         bool
	modelSwitchOrdinal       int
}

// providerTurnFrame owns output accumulated during one provider attempt. It
// never outlives runProviderTurn and does not own the stream or cancellation.
type providerTurnFrame struct {
	toolCalls                      []ToolCall
	textBuilder                    strings.Builder
	reasoningBuilder               strings.Builder
	reasoningTextItemID            string
	reasoningItemID                string
	reasoningEncryptedContent      string
	reasoningSummaryParts          []string
	reasoningKind                  ReasoningKind
	providerReplayParts            []Part
	turnMetrics                    TurnMetrics
	syncToolsExecuted              bool
	finishingToolExecuted          bool
	syncToolCalls                  []ToolCall
	syncToolResults                []Message
	syncTools                      *syncToolSupervisor
	inlineSyncParts                []Part
	scratchpadHasDiscardableOutput bool
	scratchpadCommitted            bool
	recoveryCompleted              bool
}

// engineRunTurnContext borrows run-loop state for exactly one provider turn;
// runProviderTurn writes every mutable value back before it returns.
type engineRunTurnContext struct {
	ctx                      context.Context
	req                      *Request
	send                     eventSender
	syncBridgeCtx            context.Context
	task                     *restart.Task
	checkpointErr            *error
	baseMessageCount         int
	maxTurns                 int
	planner                  ToolSurfacePlanner
	runID                    string
	modelSwitchOrdinal       *int
	originalToolChoice       ToolChoice
	originalTools            *[]ToolSpec
	restoredToolChoice       *bool
	turnCallback             TurnCompletedCallback
	responseCallback         ResponseCompletedCallback
	snapshotCallback         AssistantSnapshotCallback
	compaction               *runCompactionController
	recoveredToolWork        *bool
	recoveredToolCallIDs     *map[string]bool
	recoveredAtMessageCount  *int
	recoveryPriorErr         *error
	uncommittedStreamRetries *int
	uncommittedPriorErr      *error
	activeSyncTools          **syncToolSupervisor
	nextTurn                 *int
	providerInFlight         *bool
	sendDone                 func() error
}

func (e *Engine) runProviderTurn(state *engineRunTurnContext, attempt int) error {
	ctx, send := state.ctx, state.send
	run := &providerTurnRunFrame{
		req: *state.req, restoredToolChoice: *state.restoredToolChoice,
		recoveredToolWork: *state.recoveredToolWork, recoveredToolCallIDs: *state.recoveredToolCallIDs,
		recoveredAtMessageCount: *state.recoveredAtMessageCount, recoveryPriorErr: *state.recoveryPriorErr,
		uncommittedStreamRetries: *state.uncommittedStreamRetries, uncommittedPriorErr: *state.uncommittedPriorErr,
		activeSyncTools: *state.activeSyncTools, nextTurn: *state.nextTurn,
		providerInFlight: *state.providerInFlight, modelSwitchOrdinal: *state.modelSwitchOrdinal,
	}
	originalTools := *state.originalTools
	syncBridgeCtx, task := state.syncBridgeCtx, state.task
	checkpointErr, baseMessageCount, maxTurns := *state.checkpointErr, state.baseMessageCount, state.maxTurns
	planner, runID := state.planner, state.runID
	originalToolChoice := state.originalToolChoice
	turnCallback, responseCallback, snapshotCallback := state.turnCallback, state.responseCallback, state.snapshotCallback
	compaction := state.compaction
	compaction.req = &run.req
	requestNativeFallback := func(cause error, committed bool) (bool, string) {
		return e.requestNativeToolFallback(&run.req, planner, runID, cause, committed)
	}
	sendDone := state.sendDone
	defer func() {
		*state.req = run.req
		*state.originalTools = originalTools
		*state.restoredToolChoice = run.restoredToolChoice
		*state.recoveredToolWork = run.recoveredToolWork
		*state.recoveredToolCallIDs = run.recoveredToolCallIDs
		*state.recoveredAtMessageCount = run.recoveredAtMessageCount
		*state.recoveryPriorErr = run.recoveryPriorErr
		*state.uncommittedStreamRetries = run.uncommittedStreamRetries
		*state.uncommittedPriorErr = run.uncommittedPriorErr
		*state.activeSyncTools = run.activeSyncTools
		*state.nextTurn = run.nextTurn
		*state.providerInFlight = run.providerInFlight
		*state.modelSwitchOrdinal = run.modelSwitchOrdinal
		compaction.req = state.req
	}()
	stream, err := e.openProviderTurnStream(ctx, &run.req, send, task, checkpointErr, baseMessageCount, maxTurns, planner, runID, &run.modelSwitchOrdinal, originalToolChoice, &originalTools, compaction, requestNativeFallback, attempt, &run.providerInFlight)
	if err != nil {
		return err
	}

	// Collect tool calls and model output for this provider attempt.
	turn := &providerTurnFrame{}
	ensureSyncTools := func() *syncToolSupervisor {
		if turn.syncTools == nil {
			turn.syncTools = newSyncToolSupervisor(syncBridgeCtx, run.req.ParallelToolCalls)
			run.activeSyncTools = turn.syncTools
		}
		return turn.syncTools
	}
	absorbSyncOutcomes := func(outcomes []toolCallOutcome) {
		for _, outcome := range outcomes {
			turn.syncToolCalls = append(turn.syncToolCalls, outcome.call)
			turn.syncToolResults = append(turn.syncToolResults, outcome.message())
			if e.tools.IsFinishingTool(outcome.call.Name) {
				turn.finishingToolExecuted = true
			}
		}
	}
	foldCompletedSyncTools := func() {
		if turn.syncTools != nil {
			absorbSyncOutcomes(turn.syncTools.settleCompleted())
		}
	}
	settleSyncTools := func() {
		if turn.syncTools != nil {
			absorbSyncOutcomes(turn.syncTools.settle(ctx))
			if run.activeSyncTools == turn.syncTools {
				run.activeSyncTools = nil
			}
		}
	}
	abortSyncTools := func(cause error) {
		if turn.syncTools != nil {
			absorbSyncOutcomes(turn.syncTools.abort(cause))
			if run.activeSyncTools == turn.syncTools {
				run.activeSyncTools = nil
			}
		}
	}
	finishSyncToolsAfterStreamFailure := func(cause error) {
		if turn.syncToolsExecuted && ctx.Err() == nil {
			// Once the model has dispatched a side-effecting tool, transport
			// failure does not revoke that committed work. Drain its real outcome
			// before fallback/recovery so a retry cannot overlap detached effects.
			settleSyncTools()
			return
		}
		abortSyncTools(cause)
	}
	capabilities := e.provider.Capabilities()
	inlineToolLoop := capabilities.InlineToolLoop
	preserveInlineToolOrder := inlineToolLoop && capabilities.OrderedInlineToolEvents
	appendInlineText := func(text string) {
		if !preserveInlineToolOrder || text == "" {
			return
		}
		if n := len(turn.inlineSyncParts); n > 0 && turn.inlineSyncParts[n-1].Type == PartText {
			turn.inlineSyncParts[n-1].Text += text
			return
		}
		turn.inlineSyncParts = append(turn.inlineSyncParts, Part{Type: PartText, Text: text, CreatedAt: time.Now().UnixMilli()})
	}
	buildOrderedInlineAssistant := func(parts []Part) Message {
		return buildInterleavedAssistantMessageWithReasoningMetadata(
			parts,
			turn.reasoningBuilder.String(),
			turn.reasoningSummaryParts,
			turn.reasoningItemID,
			turn.reasoningEncryptedContent,
			turn.reasoningKind,
		)
	}
	stageOrSendModelEvent := func(event Event) error {
		event.ProviderTurnIndex = attempt
		event.ProviderTurnIndexSet = true
		if !turn.scratchpadCommitted {
			turn.scratchpadHasDiscardableOutput = true
		}
		return send.Send(event)
	}
	flushScratchpad := func() error {
		turn.scratchpadHasDiscardableOutput = false
		turn.scratchpadCommitted = true
		return nil
	}
	// buildPartialAssistant materializes the assistant message accumulated so
	// far in this turn, including the supplied tool calls.
	buildPartialAssistant := func(calls []ToolCall) Message {
		partial := ensureToolCallIDs(calls)
		partial = dedupeToolCalls(partial)
		var msg Message
		if preserveInlineToolOrder && len(partial) > 0 {
			parts := append([]Part(nil), turn.inlineSyncParts...)
			latest := e.withToolPreview(partial[len(partial)-1:])[0]
			parts = append(parts, Part{Type: PartToolCall, ToolCall: &latest, CreatedAt: time.Now().UnixMilli()})
			msg = buildOrderedInlineAssistant(parts)
		} else {
			msg = buildAssistantMessageWithReasoningMetadata(
				turn.textBuilder.String(),
				e.withToolPreview(partial),
				turn.reasoningBuilder.String(),
				turn.reasoningSummaryParts,
				turn.reasoningItemID,
				turn.reasoningEncryptedContent,
				turn.reasoningKind,
			)
		}
		return attachProviderReplayParts(msg, turn.providerReplayParts)
	}
	// fireSnapshot invokes the AssistantSnapshotCallback with the currently
	// accumulated assistant state plus the supplied tool calls. Called before
	// each EventToolCall send so consumers persist "as we go" — content
	// survives process death between emission and tool execution.
	fireSnapshot := func(calls []ToolCall) {
		if snapshotCallback == nil {
			return
		}
		msg := buildPartialAssistant(calls)
		if len(msg.Parts) == 0 {
			return
		}
		cbCtx, cancel := callbackContext(ctx)
		_ = snapshotCallback(cbCtx, attempt, msg)
		cancel()
	}
	// recoverCommittedToolWork journals completed tool-call requests and their
	// results, then continues the agent loop from the updated transcript. This
	// mirrors Codex's recovery model: if a stream dies after the model has asked
	// for a side-effecting tool, do not abandon the turn and do not retry from the
	// stale original prompt. Record the assistant tool call, execute/drain the
	// tool result, append both to run.req.Messages, and let the next loop iteration
	// continue from that journaled state.
	recovery := engineTurnRecovery{
		ctx: ctx, req: &run.req, send: send, attempt: attempt, settleSyncTools: settleSyncTools,
		toolCalls: &turn.toolCalls, syncToolsExecuted: &turn.syncToolsExecuted, recoveredToolCallIDs: &run.recoveredToolCallIDs,
		recoveredToolWork: &run.recoveredToolWork, recoveredAtMessageCount: &run.recoveredAtMessageCount, recoveryPriorErr: &run.recoveryPriorErr,
		syncToolCalls: &turn.syncToolCalls, syncToolResults: &turn.syncToolResults, preserveInlineToolOrder: preserveInlineToolOrder,
		inlineSyncParts: &turn.inlineSyncParts, buildOrderedInlineAssistant: buildOrderedInlineAssistant,
		textBuilder: &turn.textBuilder, reasoningBuilder: &turn.reasoningBuilder, reasoningSummaryParts: &turn.reasoningSummaryParts,
		reasoningItemID: &turn.reasoningItemID, reasoningEncryptedContent: &turn.reasoningEncryptedContent, reasoningKind: &turn.reasoningKind,
		providerReplayParts: &turn.providerReplayParts, compaction: compaction, turnCallback: turnCallback, turnMetrics: &turn.turnMetrics,
		runID: runID, modelSwitchOrdinal: &run.modelSwitchOrdinal, responseCallback: responseCallback,
		finishingToolExecuted: &turn.finishingToolExecuted, recoveryCompleted: &turn.recoveryCompleted, sendDone: sendDone,
	}
	retryState := engineUncommittedRetry{
		send: send, compaction: compaction, recoveredToolWork: &run.recoveredToolWork,
		toolCalls: &turn.toolCalls, syncToolsExecuted: &turn.syncToolsExecuted,
		scratchpadCommitted: &turn.scratchpadCommitted, scratchpadHasDiscardableOutput: &turn.scratchpadHasDiscardableOutput,
		retries: &run.uncommittedStreamRetries, priorErr: &run.uncommittedPriorErr,
	}

	failures := &engineStreamFailureContext{
		ctx: ctx, req: &run.req, send: send, stream: stream, compaction: compaction,
		originalTools: originalTools, originalToolChoice: originalToolChoice,
		recoveredToolWork: &run.recoveredToolWork, recoveredAtMessageCount: &run.recoveredAtMessageCount,
		toolCalls: &turn.toolCalls, syncToolCalls: &turn.syncToolCalls, textBuilder: &turn.textBuilder, reasoningBuilder: &turn.reasoningBuilder, syncToolsExecuted: &turn.syncToolsExecuted,
		scratchpadCommitted: &turn.scratchpadCommitted, scratchpadHasDiscardableOutput: &turn.scratchpadHasDiscardableOutput,
		finishSyncToolsAfterStreamFailure: finishSyncToolsAfterStreamFailure, planner: planner, runID: runID,
		retry: retryState, recovery: recovery, recoveryCompleted: &turn.recoveryCompleted,
	}
	for {
		if chaosErr := e.consumeChaosFailure(); chaosErr != nil {
			return e.handleChaosStreamFailure(failures, chaosErr)
		}
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return e.handleReceiveStreamFailure(failures, err)
		}
		if event.Type == EventError && event.Err != nil {
			return e.handleEventStreamFailure(failures, event.Err)
		}
		if run.req.DebugRaw {
			DebugRawEvent(true, event)
		}
		if event.Type == EventAttemptDiscard {
			turn.textBuilder.Reset()
			turn.reasoningBuilder.Reset()
			turn.reasoningTextItemID = ""
			turn.reasoningItemID = ""
			turn.reasoningEncryptedContent = ""
			turn.reasoningSummaryParts = nil
			turn.reasoningKind = ""
			turn.providerReplayParts = nil
			turn.turnMetrics = TurnMetrics{}
			if compaction.softActive {
				compaction.softUsage = Usage{}
				continue
			}
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		if event.Type == EventToolActivity {
			if event.ToolActivity != nil {
				turn.providerReplayParts = upsertToolActivityPart(turn.providerReplayParts, event.ToolActivity)
				fireSnapshot(turn.toolCalls)
			}
			continue
		}
		if event.Type == EventProviderReplay {
			if event.ProviderReplay != nil && len(event.ProviderReplay.Raw) > 0 {
				replay := &ProviderReplayItem{Raw: append(json.RawMessage(nil), event.ProviderReplay.Raw...)}
				turn.providerReplayParts = append(turn.providerReplayParts, Part{Type: PartProviderReplay, ProviderReplay: replay})
			}
			continue
		}
		if event.Type == EventDiscoveryCall && event.DiscoveryCall != nil {
			if compaction.softActive {
				return fmt.Errorf("provider emitted native tool discovery during internal compaction")
			}
			nativePlanner, ok := planner.(NativeToolDiscoveryPlanner)
			if !ok || run.req.NativeToolDiscovery == nil {
				return fmt.Errorf("provider emitted native tool discovery without active planner support")
			}
			if err := flushScratchpad(); err != nil {
				return err
			}
			call := ToolDiscoveryCall{ID: event.DiscoveryCall.ID, Arguments: append(json.RawMessage(nil), event.DiscoveryCall.Arguments...)}
			output, err := nativePlanner.ResolveNativeToolDiscovery(ctx, runID, call)
			if err != nil {
				return fmt.Errorf("resolve native tool discovery: %w", err)
			}
			turn.providerReplayParts = append(turn.providerReplayParts,
				Part{Type: PartDiscoveryCall, DiscoveryCall: &call},
				Part{Type: PartDiscoveryOutput, DiscoveryOutput: cloneToolDiscoveryOutput(&output)},
			)
			if err := send.Send(Event{Type: EventDiscoveryCall, DiscoveryCall: &call}); err != nil {
				return err
			}
			if err := send.Send(Event{Type: EventDiscoveryOutput, DiscoveryOutput: cloneToolDiscoveryOutput(&output)}); err != nil {
				return err
			}
			fireSnapshot(turn.toolCalls)
			continue
		}
		// Track usage metrics
		if event.Type == EventUsage && event.Use != nil {
			if compaction.softActive {
				compaction.softUsage.Add(*event.Use)
			} else {
				turn.turnMetrics.InputTokens += event.Use.InputTokens
				turn.turnMetrics.OutputTokens += event.Use.OutputTokens
				turn.turnMetrics.CachedInputTokens += event.Use.CachedInputTokens
				turn.turnMetrics.CacheWriteTokens += event.Use.CacheWriteTokens
			}
			// Update token tracking for compaction threshold and status line display.
			// InputTokens is the non-cached portion; CachedInputTokens is the cached
			// portion. Together they equal the total context size this turn. Adding
			// OutputTokens gives the baseline for the next turn's input estimate
			// (the model's output becomes assistant-message input on the next turn).
			// All providers normalise to this convention — see Usage type docs.
			if compaction.inputLimit > 0 {
				messageCount := len(run.req.Messages)
				// Usage total includes the assistant output from this provider turn.
				// That output becomes assistant-message input on the next request, so
				// retain a compatibility count that includes the in-flight assistant
				// row when this usage event carries provider output.
				if event.Use.OutputTokens > 0 || turn.textBuilder.Len() > 0 || turn.reasoningBuilder.Len() > 0 || len(turn.toolCalls) > 0 || len(turn.syncToolCalls) > 0 || turn.reasoningItemID != "" || turn.reasoningEncryptedContent != "" {
					messageCount++
				}
				e.callbackMu.Lock()
				e.lastTotalTokens = event.Use.InputTokens + event.Use.CachedInputTokens + event.Use.OutputTokens
				e.lastMessageCount = messageCount
				e.callbackMu.Unlock()
			}
			if compaction.softActive {
				continue
			}
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		// Accumulate text for callback
		if event.Type == EventTextDelta && event.Text != "" {
			turn.textBuilder.WriteString(event.Text)
			appendInlineText(event.Text)
			if compaction.softActive {
				continue
			}
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		// Accumulate reasoning for thinking models (OpenRouter)
		if event.Type == EventReasoningDelta {
			internalreasoning.AppendStreamItemText(&turn.reasoningBuilder, &turn.reasoningTextItemID, event.Text, event.ReasoningItemID)
			if event.Text != "" {
				turn.reasoningKind = MergeReasoningKind(turn.reasoningKind, event.ReasoningKind)
			}
		}
		if event.Type == EventReasoningDelta {
			if len(event.ReasoningSummaryParts) > 0 {
				turn.reasoningSummaryParts = append([]string(nil), event.ReasoningSummaryParts...)
				turn.reasoningKind = MergeReasoningKind(turn.reasoningKind, ReasoningKindSummary)
			}
			if event.ReasoningItemID != "" {
				turn.reasoningItemID = event.ReasoningItemID
			}
			if event.ReasoningEncryptedContent != "" {
				turn.reasoningEncryptedContent = event.ReasoningEncryptedContent
				turn.reasoningKind = MergeReasoningKind(turn.reasoningKind, event.ReasoningKind)
			}
			if compaction.softActive {
				continue
			}
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		if event.Type == EventToolCall && compaction.softActive {
			// The continuation-brief turn is internal and explicitly forbids tools.
			// If a provider ignores tool_choice=none, discard the tool call and let
			// the no-brief fallback compact hard at the end of the stream.
			continue
		}
		if event.Type == EventToolCall && event.Tool != nil {
			if event.Tool.Namespace != "" {
				if planner == nil {
					return fmt.Errorf("provider emitted namespace tool call %q/%q without an active discovery planner", event.Tool.Namespace, event.Tool.ChildName)
				}
				resolved, err := planner.ResolveProviderToolCall(runID, *event.Tool)
				if err != nil {
					return fmt.Errorf("resolve provider namespace tool call: %w", err)
				}
				event.Tool = &resolved
				event.ToolName = resolved.Name
			}
			// Normalize every provider path before snapshotting, forwarding, or
			// execution. Tool.ID is canonical because it is persisted, executed,
			// and echoed to provider protocols; synthesize it only when the
			// provider omitted both representations. Copy the provider-owned value
			// before normalizing it.
			toolCall := *event.Tool
			toolCallID := toolCall.ID
			if strings.TrimSpace(toolCallID) == "" {
				toolCallID = event.ToolCallID
			}
			if strings.TrimSpace(toolCallID) == "" {
				toolCallID = newSyntheticToolCallID()
			}
			toolCall.ID = toolCallID
			event.Tool = &toolCall
			event.ToolCallID = toolCallID
			if err := flushScratchpad(); err != nil {
				return err
			}
			// Check if this is a synchronous tool execution request from a provider bridge.
			if event.ToolResponse != nil {
				// Forward the EventToolCall so consumers can see tool calls (e.g., exec.go needs
				// to see suggest_commands calls to parse suggestions from the arguments).
				// Create a copy without ToolResponse to avoid confusion.
				forwardEvent := Event{
					Type:                 EventToolCall,
					ToolCallID:           event.ToolCallID,
					ToolName:             event.ToolName,
					Tool:                 event.Tool,
					ProviderTurnIndex:    attempt,
					ProviderTurnIndexSet: true,
				}
				supervisor := ensureSyncTools()
				foldCompletedSyncTools()
				pendingSyncCalls := append(append([]ToolCall(nil), turn.syncToolCalls...), supervisor.pendingCalls()...)
				pendingSyncCalls = append(pendingSyncCalls, *event.Tool)
				fireSnapshot(pendingSyncCalls)
				if err := send.Send(forwardEvent); err != nil {
					return err
				}

				// Synchronous CLI requests are supervised children of this provider
				// turn. Dispatch never blocks the event loop, allowing sibling MCP
				// calls to reach the same executor concurrently.
				call := *event.Tool
				call.ToolInfo = e.getToolPreview(call)
				if event.ToolInfo != "" {
					call.ToolInfo = event.ToolInfo
				}
				committedResults := append([]Message(nil), turn.syncToolResults...)
				approvalAssistant := buildPartialAssistant(pendingSyncCalls)
				turn.syncToolsExecuted = true
				if preserveInlineToolOrder {
					inlineCall := call
					turn.inlineSyncParts = append(turn.inlineSyncParts, Part{Type: PartToolCall, ToolCall: &inlineCall, CreatedAt: time.Now().UnixMilli()})
				}
				response := event.ToolResponse
				supervisor.dispatch(call, func(toolCtx context.Context) toolCallOutcome {
					_, evidenceOutcomes := supervisor.evidenceFor(call.ID)
					approvalResults := append([]Message(nil), committedResults...)
					for _, prior := range evidenceOutcomes {
						approvalResults = append(approvalResults, prior.message())
					}
					transcript := buildApprovalTranscript(
						run.req.ApprovalTranscriptPrefix, run.req.Messages, approvalAssistant, approvalResults...)
					send.TrySend(Event{Type: EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: call.ToolInfo, ToolArgs: call.Arguments})
					return e.executeSingleToolCallOutcomeSafe(
						ContextWithApprovalTranscript(toolCtx, transcript), call, send, run.req.Debug, run.req.DebugRaw)
				}, func(outcome toolCallOutcome) {
					select {
					case response <- ToolExecutionResponse{Result: outcome.output, Err: outcome.err}:
					case <-ctx.Done():
					}
				})
				continue
			}
			// Normal async collection for other providers.
			info := event.ToolInfo
			if info == "" {
				info = event.Tool.ToolInfo
			}
			if info == "" {
				info = e.getToolPreview(*event.Tool)
			}
			event.Tool.ToolInfo = info
			turn.toolCalls = append(turn.toolCalls, *event.Tool)

			fireSnapshot(turn.toolCalls)
			if err := send.Send(Event{
				Type:                 EventToolCall,
				ToolCallID:           toolCallID,
				ToolName:             event.Tool.Name,
				Tool:                 event.Tool,
				ToolInfo:             info,
				ProviderTurnIndex:    attempt,
				ProviderTurnIndexSet: true,
			}); err != nil {
				return err
			}
			continue
		}
		if event.Type == EventImageGenerated {
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		if event.Type == EventDone {
			continue
		}
		if err := send.Send(event); err != nil {
			return err
		}
	}
	settleSyncTools()
	stream.Close()
	if ctx.Err() == nil {
		run.providerInFlight = false
	}

	// Exit promptly if caller cancelled while we were streaming.
	if err := ctx.Err(); err != nil {
		return err
	}

	// The stream reached its provider-defined end without an error. Commit any
	// attempt-local assistant output before callbacks and final done/tool handling.
	if err := flushScratchpad(); err != nil {
		return err
	}

	// Search is only performed once (either pre-emptively or in first turn)
	run.req.Search = false

	completion := &engineTurnCompletion{
		ctx: ctx, send: send, req: &run.req, attempt: attempt, maxTurns: maxTurns, runID: runID,
		modelSwitchOrdinal: &run.modelSwitchOrdinal, nextTurn: &run.nextTurn, restoredToolChoice: &run.restoredToolChoice,
		originalToolChoice: originalToolChoice, originalTools: originalTools, turnCallback: turnCallback,
		responseCallback: responseCallback, compaction: compaction, sendDone: sendDone, task: task,
		checkpointErr: state.checkpointErr, baseMessageCount: baseMessageCount, toolCalls: turn.toolCalls,
		syncToolCalls: turn.syncToolCalls, syncToolResults: turn.syncToolResults, textBuilder: &turn.textBuilder,
		reasoningBuilder: &turn.reasoningBuilder, reasoningSummaryParts: turn.reasoningSummaryParts,
		reasoningItemID: turn.reasoningItemID, reasoningEncryptedContent: turn.reasoningEncryptedContent,
		reasoningKind: turn.reasoningKind, providerReplayParts: turn.providerReplayParts, inlineSyncParts: turn.inlineSyncParts,
		turnMetrics: turn.turnMetrics, buildOrderedInlineAssistant: buildOrderedInlineAssistant,
		finishingToolExecuted: turn.finishingToolExecuted, preserveInlineToolOrder: preserveInlineToolOrder,
		syncToolsExecuted: turn.syncToolsExecuted, inlineToolLoop: inlineToolLoop,
		recoveredToolWork: run.recoveredToolWork, recoveryPriorErr: run.recoveryPriorErr, uncommittedPriorErr: run.uncommittedPriorErr,
	}
	if handled, err := e.completeTextTurn(completion); handled {
		return err
	}
	if handled, err := e.completeSyncTurn(completion); handled {
		return err
	}
	return e.completeAsyncTurn(completion)
}
