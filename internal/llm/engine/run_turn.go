package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
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
	req                      core.Request
	forcedChoice             forcedChoiceState
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
	toolCalls                      []core.ToolCall
	textBuilder                    strings.Builder
	reasoningBuilder               strings.Builder
	reasoningTextItemID            string
	reasoningItemID                string
	reasoningEncryptedContent      string
	reasoningSummaryParts          []string
	reasoningKind                  core.ReasoningKind
	providerReplayParts            []core.Part
	turnMetrics                    core.TurnMetrics
	syncToolsExecuted              bool
	finishingToolExecuted          bool
	syncToolCalls                  []core.ToolCall
	syncToolResults                []core.Message
	syncTools                      *syncToolSupervisor
	inlineSyncParts                []core.Part
	scratchpadHasDiscardableOutput bool
	scratchpadCommitted            bool
	recoveryCompleted              bool
}

type providerTurnSyncTools struct {
	engine    *Engine
	ctx       context.Context
	bridgeCtx context.Context
	run       *providerTurnRunFrame
	turn      *providerTurnFrame
}

func (s *providerTurnSyncTools) ensure() *syncToolSupervisor {
	if s.turn.syncTools == nil {
		s.turn.syncTools = newSyncToolSupervisor(s.bridgeCtx, s.run.req.ParallelToolCalls)
		s.run.activeSyncTools = s.turn.syncTools
	}
	return s.turn.syncTools
}

func (s *providerTurnSyncTools) absorb(outcomes []toolCallOutcome) {
	for _, outcome := range outcomes {
		s.turn.syncToolCalls = append(s.turn.syncToolCalls, outcome.call)
		s.turn.syncToolResults = append(s.turn.syncToolResults, outcome.message())
		if s.engine.tools.IsFinishingTool(outcome.call.Name) {
			s.turn.finishingToolExecuted = true
		}
	}
}

func (s *providerTurnSyncTools) foldCompleted() {
	if s.turn.syncTools != nil {
		s.absorb(s.turn.syncTools.settleCompleted())
	}
}

func (s *providerTurnSyncTools) settle() {
	if s.turn.syncTools == nil {
		return
	}
	s.absorb(s.turn.syncTools.settle(s.ctx))
	if s.run.activeSyncTools == s.turn.syncTools {
		s.run.activeSyncTools = nil
	}
}

func (s *providerTurnSyncTools) abort(cause error) {
	if s.turn.syncTools == nil {
		return
	}
	s.absorb(s.turn.syncTools.abort(cause))
	if s.run.activeSyncTools == s.turn.syncTools {
		s.run.activeSyncTools = nil
	}
}

func (s *providerTurnSyncTools) finishAfterStreamFailure(cause error) {
	// Once a side-effecting synchronous tool was dispatched, a transport failure
	// cannot revoke it. Drain its real outcome before fallback/retry so detached
	// effects never overlap a second execution of the same turn.
	if s.turn.syncToolsExecuted && s.ctx.Err() == nil {
		s.settle()
		return
	}
	s.abort(cause)
}

type providerTurnToolEvents struct {
	engine                  *Engine
	ctx                     context.Context
	send                    corestream.Sender
	run                     *providerTurnRunFrame
	turn                    *providerTurnFrame
	syncTools               *providerTurnSyncTools
	planner                 core.ToolSurfacePlanner
	runID                   string
	attempt                 int
	preserveInlineToolOrder bool
	flushScratchpad         func() error
	fireSnapshot            func([]core.ToolCall)
	buildPartialAssistant   func([]core.ToolCall) core.Message
}

func (h *providerTurnToolEvents) handle(event core.Event, softCompaction bool) (bool, error) {
	if event.Type != core.EventToolCall {
		return false, nil
	}
	if softCompaction {
		return true, nil
	}
	if event.Tool == nil {
		return false, nil
	}
	if err := h.resolveAndNormalize(&event); err != nil {
		return true, err
	}
	if err := h.flushScratchpad(); err != nil {
		return true, err
	}
	if event.ToolResponse != nil {
		return true, h.handleSync(event)
	}
	return true, h.handleAsync(event)
}

func (h *providerTurnToolEvents) resolveAndNormalize(event *core.Event) error {
	if event.Tool.Namespace != "" {
		if h.planner == nil {
			return fmt.Errorf("provider emitted namespace tool call %q/%q without an active discovery planner", event.Tool.Namespace, event.Tool.ChildName)
		}
		resolved, err := h.planner.ResolveProviderToolCall(h.runID, *event.Tool)
		if err != nil {
			return fmt.Errorf("resolve provider namespace tool call: %w", err)
		}
		event.Tool = &resolved
		event.ToolName = resolved.Name
	}
	call := *event.Tool
	callID := call.ID
	if strings.TrimSpace(callID) == "" {
		callID = event.ToolCallID
	}
	if strings.TrimSpace(callID) == "" {
		callID = core.NewSyntheticToolCallID()
	}
	call.ID = callID
	event.Tool, event.ToolCallID = &call, callID
	return nil
}

func (h *providerTurnToolEvents) handleSync(event core.Event) error {
	forward := core.Event{Type: core.EventToolCall, ToolCallID: event.ToolCallID, ToolName: event.ToolName, Tool: event.Tool, ToolInline: true, ProviderTurnIndex: h.attempt, ProviderTurnIndexSet: true}
	supervisor := h.syncTools.ensure()
	h.syncTools.foldCompleted()
	pending := append(append([]core.ToolCall(nil), h.turn.syncToolCalls...), supervisor.pendingCalls()...)
	pending = append(pending, *event.Tool)
	h.fireSnapshot(pending)
	if err := h.send.Send(forward); err != nil {
		return err
	}
	call := *event.Tool
	call.ToolInfo = h.engine.getToolPreview(call)
	if event.ToolInfo != "" {
		call.ToolInfo = event.ToolInfo
	}
	committedResults := append([]core.Message(nil), h.turn.syncToolResults...)
	approvalAssistant := h.buildPartialAssistant(pending)
	h.turn.syncToolsExecuted = true
	if h.preserveInlineToolOrder {
		inlineCall := call
		h.turn.inlineSyncParts = append(h.turn.inlineSyncParts, core.Part{Type: core.PartToolCall, ToolCall: &inlineCall, CreatedAt: time.Now().UnixMilli()})
	}
	response := event.ToolResponse
	supervisor.dispatch(call, func(toolCtx context.Context) toolCallOutcome {
		_, evidence := supervisor.evidenceFor(call.ID)
		approvalResults := append([]core.Message(nil), committedResults...)
		for _, prior := range evidence {
			approvalResults = append(approvalResults, prior.message())
		}
		transcript := buildApprovalTranscript(h.run.req.ApprovalTranscriptPrefix, h.run.req.Messages, approvalAssistant, approvalResults...)
		h.send.TrySend(core.Event{Type: core.EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: call.ToolInfo, ToolArgs: call.Arguments})
		return h.engine.executeSingleToolCallOutcomeSafe(core.ContextWithApprovalTranscript(toolCtx, transcript), call, h.send, h.run.req.Debug, h.run.req.DebugRaw)
	}, func(outcome toolCallOutcome) {
		select {
		case response <- core.ToolExecutionResponse{Result: outcome.output, Err: outcome.err}:
		case <-h.ctx.Done():
		}
	})
	return nil
}

func (h *providerTurnToolEvents) handleAsync(event core.Event) error {
	info := event.ToolInfo
	if info == "" {
		info = event.Tool.ToolInfo
	}
	if info == "" {
		info = h.engine.getToolPreview(*event.Tool)
	}
	event.Tool.ToolInfo = info
	h.turn.toolCalls = append(h.turn.toolCalls, *event.Tool)
	h.fireSnapshot(h.turn.toolCalls)
	return h.send.Send(core.Event{Type: core.EventToolCall, ToolCallID: event.ToolCallID, ToolName: event.Tool.Name, Tool: event.Tool, ToolInfo: info, ProviderTurnIndex: h.attempt, ProviderTurnIndexSet: true})
}

// engineRunTurnContext borrows run-loop state for exactly one provider turn;
// runProviderTurn writes every mutable value back before it returns.
type engineRunTurnContext struct {
	ctx                      context.Context
	req                      *core.Request
	send                     corestream.Sender
	syncBridgeCtx            context.Context
	task                     *restart.Task
	checkpointErr            *error
	baseMessageCount         int
	maxTurns                 int
	planner                  core.ToolSurfacePlanner
	runID                    string
	modelSwitchOrdinal       *int
	originalToolChoice       core.ToolChoice
	originalTools            *[]core.ToolSpec
	forcedChoice             *forcedChoiceState
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
		req: *state.req, forcedChoice: *state.forcedChoice,
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
		*state.forcedChoice = run.forcedChoice
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
	// Tool work recovered after a stream failure satisfies a forced tool_choice.
	// Recovery advances without completing the turn, so settle a pending forced
	// retry here, before the next turn's tool_choice is chosen.
	if run.recoveredToolWork && run.forcedChoice == forcedChoiceRetryNext {
		run.forcedChoice = forcedChoiceSpent
	}
	stream, err := e.openProviderTurnStream(ctx, &run.req, send, task, checkpointErr, baseMessageCount, maxTurns, planner, runID, &run.modelSwitchOrdinal, originalToolChoice, &run.forcedChoice, &originalTools, compaction, requestNativeFallback, attempt, &run.providerInFlight)
	if err != nil {
		return err
	}

	// Collect tool calls and model output for this provider attempt.
	turn := &providerTurnFrame{}
	syncTools := &providerTurnSyncTools{engine: e, ctx: ctx, bridgeCtx: syncBridgeCtx, run: run, turn: turn}
	capabilities := e.provider.Capabilities()
	inlineToolLoop := capabilities.InlineToolLoop
	preserveInlineToolOrder := inlineToolLoop && capabilities.OrderedInlineToolEvents
	appendInlineText := func(text string) {
		if !preserveInlineToolOrder || text == "" {
			return
		}
		if n := len(turn.inlineSyncParts); n > 0 && turn.inlineSyncParts[n-1].Type == core.PartText {
			turn.inlineSyncParts[n-1].Text += text
			return
		}
		turn.inlineSyncParts = append(turn.inlineSyncParts, core.Part{Type: core.PartText, Text: text, CreatedAt: time.Now().UnixMilli()})
	}
	buildOrderedInlineAssistant := func(parts []core.Part) core.Message {
		return buildInterleavedAssistantMessageWithReasoningMetadata(
			parts,
			turn.reasoningBuilder.String(),
			turn.reasoningSummaryParts,
			turn.reasoningItemID,
			turn.reasoningEncryptedContent,
			turn.reasoningKind,
		)
	}
	stageOrSendModelEvent := func(event core.Event) error {
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
	buildPartialAssistant := func(calls []core.ToolCall) core.Message {
		partial := ensureToolCallIDs(calls)
		partial = dedupeToolCalls(partial)
		var msg core.Message
		if preserveInlineToolOrder && len(partial) > 0 {
			parts := append([]core.Part(nil), turn.inlineSyncParts...)
			latest := e.withToolPreview(partial[len(partial)-1:])[0]
			parts = append(parts, core.Part{Type: core.PartToolCall, ToolCall: &latest, CreatedAt: time.Now().UnixMilli()})
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
	fireSnapshot := func(calls []core.ToolCall) {
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
		ctx: ctx, req: &run.req, send: send, attempt: attempt, settleSyncTools: syncTools.settle,
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
		finishSyncToolsAfterStreamFailure: syncTools.finishAfterStreamFailure, planner: planner, runID: runID,
		retry: retryState, recovery: recovery, recoveryCompleted: &turn.recoveryCompleted,
	}
	toolEvents := &providerTurnToolEvents{
		engine: e, ctx: ctx, send: send, run: run, turn: turn, syncTools: syncTools,
		planner: planner, runID: runID, attempt: attempt, preserveInlineToolOrder: preserveInlineToolOrder,
		flushScratchpad: flushScratchpad, fireSnapshot: fireSnapshot, buildPartialAssistant: buildPartialAssistant,
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
		if event.Type == core.EventError && event.Err != nil {
			return e.handleEventStreamFailure(failures, event.Err)
		}
		if run.req.DebugRaw {
			env.debugRawEvent(true, event)
		}
		if event.Type == core.EventAttemptDiscard {
			turn.textBuilder.Reset()
			turn.reasoningBuilder.Reset()
			turn.reasoningTextItemID = ""
			turn.reasoningItemID = ""
			turn.reasoningEncryptedContent = ""
			turn.reasoningSummaryParts = nil
			turn.reasoningKind = ""
			turn.providerReplayParts = nil
			turn.turnMetrics = core.TurnMetrics{}
			if compaction.softActive {
				compaction.softUsage = core.Usage{}
				continue
			}
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		if event.Type == core.EventToolActivity {
			if event.ToolActivity != nil {
				turn.providerReplayParts = upsertToolActivityPart(turn.providerReplayParts, event.ToolActivity)
				fireSnapshot(turn.toolCalls)
			}
			continue
		}
		if event.Type == core.EventProviderReplay {
			if event.ProviderReplay != nil && len(event.ProviderReplay.Raw) > 0 {
				replay := &core.ProviderReplayItem{Raw: append(json.RawMessage(nil), event.ProviderReplay.Raw...)}
				turn.providerReplayParts = append(turn.providerReplayParts, core.Part{Type: core.PartProviderReplay, ProviderReplay: replay})
			}
			continue
		}
		if event.Type == core.EventDiscoveryCall && event.DiscoveryCall != nil {
			if compaction.softActive {
				return fmt.Errorf("provider emitted native tool discovery during internal compaction")
			}
			nativePlanner, ok := planner.(core.NativeToolDiscoveryPlanner)
			if !ok || run.req.NativeToolDiscovery == nil {
				return fmt.Errorf("provider emitted native tool discovery without active planner support")
			}
			if err := flushScratchpad(); err != nil {
				return err
			}
			call := core.ToolDiscoveryCall{ID: event.DiscoveryCall.ID, Arguments: append(json.RawMessage(nil), event.DiscoveryCall.Arguments...)}
			output, err := nativePlanner.ResolveNativeToolDiscovery(ctx, runID, call)
			if err != nil {
				return fmt.Errorf("resolve native tool discovery: %w", err)
			}
			turn.providerReplayParts = append(turn.providerReplayParts,
				core.Part{Type: core.PartDiscoveryCall, DiscoveryCall: &call},
				core.Part{Type: core.PartDiscoveryOutput, DiscoveryOutput: core.CloneToolDiscoveryOutput(&output)},
			)
			if err := send.Send(core.Event{Type: core.EventDiscoveryCall, DiscoveryCall: &call}); err != nil {
				return err
			}
			if err := send.Send(core.Event{Type: core.EventDiscoveryOutput, DiscoveryOutput: core.CloneToolDiscoveryOutput(&output)}); err != nil {
				return err
			}
			fireSnapshot(turn.toolCalls)
			continue
		}
		// Track usage metrics
		if event.Type == core.EventUsage && event.Use != nil {
			if compaction.softActive {
				compaction.softUsage.Add(*event.Use)
			} else {
				turn.turnMetrics.InputTokens += event.Use.InputTokens
				turn.turnMetrics.OutputTokens += event.Use.OutputTokens
				turn.turnMetrics.CachedInputTokens += event.Use.CachedInputTokens
				turn.turnMetrics.CacheWriteTokens += event.Use.CacheWriteTokens
			}
			// Update token tracking for compaction threshold and status line display.
			// All providers normalise usage to additive input/cached/cache-write
			// counters — see Usage type docs.
			e.recordUsageBaseline(compaction.inputLimit, event.Use, len(run.req.Messages), turn.hasProvisionalOutput())
			if compaction.softActive {
				continue
			}
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		// Accumulate text for callback
		if event.Type == core.EventTextDelta && event.Text != "" {
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
		if event.Type == core.EventReasoningDelta {
			internalreasoning.AppendStreamItemText(&turn.reasoningBuilder, &turn.reasoningTextItemID, event.Text, event.ReasoningItemID)
			if event.Text != "" {
				turn.reasoningKind = core.MergeReasoningKind(turn.reasoningKind, event.ReasoningKind)
			}
		}
		if event.Type == core.EventReasoningDelta {
			if len(event.ReasoningSummaryParts) > 0 {
				turn.reasoningSummaryParts = append([]string(nil), event.ReasoningSummaryParts...)
				turn.reasoningKind = core.MergeReasoningKind(turn.reasoningKind, core.ReasoningKindSummary)
			}
			if event.ReasoningItemID != "" {
				turn.reasoningItemID = event.ReasoningItemID
			}
			if event.ReasoningEncryptedContent != "" {
				turn.reasoningEncryptedContent = event.ReasoningEncryptedContent
				turn.reasoningKind = core.MergeReasoningKind(turn.reasoningKind, event.ReasoningKind)
			}
			if compaction.softActive {
				continue
			}
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		if handled, err := toolEvents.handle(event, compaction.softActive); handled {
			if err != nil {
				return err
			}
			continue
		}
		if event.Type == core.EventImageGenerated {
			if err := stageOrSendModelEvent(event); err != nil {
				return err
			}
			continue
		}
		if event.Type == core.EventDone {
			continue
		}
		e.observeReportedContext(event)
		if err := send.Send(event); err != nil {
			return err
		}
	}
	syncTools.settle()
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
		modelSwitchOrdinal: &run.modelSwitchOrdinal, nextTurn: &run.nextTurn, forcedChoice: &run.forcedChoice,
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
