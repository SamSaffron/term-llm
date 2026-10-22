package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"strings"

	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func (e *Engine) runLoop(ctx context.Context, req core.Request, send corestream.Sender) (returnErr error) {
	ctx = core.ScopeClientToolRunner(ctx, req.Tools)
	syncBridgeCtx, cancelSyncBridges := context.WithCancel(ctx)
	defer cancelSyncBridges()
	task := restart.CurrentTask(ctx)
	if req.Ephemeral || (task != nil && !task.Claim(e)) {
		task = nil
	}
	if task != nil && e.provider.Capabilities().InlineToolLoop {
		if flusher, ok := e.provider.(interface{ RequestReloadFlush() }); ok {
			task.OnRequest(func() {
				if task.Pending() {
					flusher.RequestReloadFlush()
				}
			})
			defer task.OnRequest(nil)
		}
	}
	defer task.Release(e)
	ctx, finishStep := task.Step(ctx)
	defer finishStep()
	resume := req.Resume
	req.Resume = nil
	nextTurn := 0
	baseMessageCount := len(req.Messages)
	providerInFlight := false
	if resume != nil {
		nextTurn = resume.Turn
		baseMessageCount = resume.BaseMessageCount
	}
	var checkpointErr error
	defer func() {
		var alreadySuspended *SuspendedError
		if returnErr == nil || errors.As(returnErr, &alreadySuspended) {
			return
		}
		if task.Pending() && interruptedForRestart(ctx) && !e.provider.Capabilities().InlineToolLoop {
			e.waitActualTools()
			if checkpointErr != nil {
				returnErr = checkpointErr
				return
			}
			returnErr = e.suspend(req, nextTurn, nil, true, baseMessageCount)
			var checkpoint *SuspendedError
			if providerInFlight && errors.As(returnErr, &checkpoint) {
				checkpoint.Continuation.DiscardPartial = true
				_ = send.Send(core.Event{Type: core.EventAttemptDiscard, ProviderTurnIndex: nextTurn, ProviderTurnIndexSet: true})
			}
		}
	}()
	ctx = env.withRunContext(ctx)
	defer e.markSteeringRunNonConsuming()
	runID := e.beginToolRun()
	modelSwitchOrdinal := 0
	e.recordFileTrackingRunStart(ctx, req.SessionID, runID)
	defer func() {
		e.recordFileTrackingRunComplete(ctx, req.SessionID, runID)
		e.endToolRun(runID)
	}()
	ctx = core.ContextWithToolRunID(ctx, runID)
	// Bridge tools intentionally outlive restart step cancellation, but must
	// carry the same tracking identity as ordinary engine-managed tools.
	syncBridgeCtx = core.ContextWithToolRunID(syncBridgeCtx, runID)
	var planner core.ToolSurfacePlanner
	if req.EnableToolDiscovery {
		planner = e.currentToolPlanner()
	}
	if planner != nil {
		defer planner.EndRun(runID)
		resetReason, err := planner.BeginRun(ctx, e.provider, &req, runID)
		if err != nil {
			return fmt.Errorf("prepare tool discovery: %w", err)
		}
		if resetReason != "" {
			resetProviderConversation(e.provider)
			runtimeoutput.Debug("reset provider conversation for tool-surface change", "reason", resetReason, "session_id", req.SessionID)
		}
	}
	sendDone := func() error {
		e.markSteeringRunNonConsuming()
		return send.Send(core.Event{Type: core.EventDone})
	}
	maxTurns := getMaxTurns(req)
	originalToolChoice := req.ToolChoice
	originalTools := append([]core.ToolSpec(nil), req.Tools...)
	forcedChoice := forcedChoiceUnspent

	// Snapshot callbacks and compaction config at start — protects against
	// concurrent modification from the UI thread (e.g., SetCompaction called
	// from a new startStream while a previous stream is finishing).
	turnCallback := e.getTurnCallback()
	responseCallback := e.getResponseCallback()
	snapshotCallback := e.getSnapshotCallback()
	if task != nil {
		if persist := turnCallback; persist != nil {
			turnCallback = func(ctx context.Context, turn int, messages []core.Message, metrics core.TurnMetrics) error {
				err := persist(ctx, turn, messages, metrics)
				if err != nil && checkpointErr == nil {
					checkpointErr = err
				}
				return err
			}
		}
		if persist := responseCallback; persist != nil {
			responseCallback = func(ctx context.Context, turn int, message core.Message, metrics core.TurnMetrics) error {
				err := persist(ctx, turn, message, metrics)
				if err != nil && checkpointErr == nil {
					checkpointErr = err
				}
				return err
			}
		}
	}

	compaction := newRunCompactionController(ctx, e, &req, send)
	// Every compaction span is closed before the run returns. Explicit success
	// and failure paths already emit their own end; this only catches an
	// attempt that was still in flight (cancellation, suspension, or an
	// unexpected exit) so no consumer can observe a permanently active
	// compaction for a finished run.
	defer func() {
		cause := returnErr
		if cause == nil {
			cause = ctx.Err()
		}
		compaction.lifecycle.finish(cause)
	}()
	var recoveredToolWork bool
	var recoveredToolCallIDs map[string]bool
	var recoveredAtMessageCount = -1
	var recoveryPriorErr error
	var uncommittedStreamRetries int
	var uncommittedPriorErr error

	if resume != nil && len(resume.Pending) > 0 {
		if task.Pending() {
			return e.suspend(req, resume.Turn, resume.Pending, resume.Interrupted, baseMessageCount, resume.PendingMetrics)
		}
		nextTurn = resume.Turn + 1
		if err := e.resumePending(ctx, &req, resume, send, turnCallback); err != nil {
			return err
		}
		for _, call := range resume.Pending {
			name := call.Name
			if mapped := req.ToolMap[name]; mapped != "" {
				name = mapped
			}
			if e.tools.IsFinishingTool(name) {
				return sendDone()
			}
		}
	}
	var activeSyncTools *syncToolSupervisor
	defer func() {
		if activeSyncTools == nil {
			return
		}
		cause := returnErr
		if cause == nil {
			cause = context.Canceled
		}
		activeSyncTools.abort(cause)
	}()

	for attempt := nextTurn; attempt < maxTurns; attempt++ {
		nextTurn = attempt
		turnErr := e.runProviderTurn(&engineRunTurnContext{
			ctx: ctx, req: &req, send: send, syncBridgeCtx: syncBridgeCtx, task: task,
			checkpointErr: &checkpointErr, baseMessageCount: baseMessageCount, maxTurns: maxTurns,
			planner: planner, runID: runID, modelSwitchOrdinal: &modelSwitchOrdinal,
			originalToolChoice: originalToolChoice, originalTools: &originalTools, forcedChoice: &forcedChoice,
			turnCallback: turnCallback, responseCallback: responseCallback, snapshotCallback: snapshotCallback,
			compaction: compaction, recoveredToolWork: &recoveredToolWork, recoveredToolCallIDs: &recoveredToolCallIDs,
			recoveredAtMessageCount: &recoveredAtMessageCount, recoveryPriorErr: &recoveryPriorErr,
			uncommittedStreamRetries: &uncommittedStreamRetries, uncommittedPriorErr: &uncommittedPriorErr,
			activeSyncTools: &activeSyncTools, nextTurn: &nextTurn, providerInFlight: &providerInFlight,
			sendDone: sendDone,
		}, attempt)
		if errors.Is(turnErr, errEngineTurnRetry) {
			attempt--
			continue
		}
		if errors.Is(turnErr, errEngineTurnAdvance) {
			continue
		}
		return turnErr
	}

	return fmt.Errorf("agentic loop ended unexpectedly")
}

// buildAssistantMessage creates an assistant message with text, tool calls, and optional reasoning.
// The reasoning parameter is for thinking models (OpenRouter reasoning_content).
func buildAssistantMessage(text string, toolCalls []core.ToolCall, reasoning string) core.Message {
	return buildAssistantMessageWithReasoningMetadata(text, toolCalls, reasoning, nil, "", "", core.ReasoningKindUnknown)
}

func buildAssistantMessageWithReasoningMetadata(text string, toolCalls []core.ToolCall, reasoning string, reasoningSummaryParts []string, reasoningItemID, reasoningEncryptedContent string, reasoningKind core.ReasoningKind) core.Message {
	var parts []core.Part
	if text != "" || reasoning != "" || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" {
		if reasoning == "" && len(reasoningSummaryParts) > 0 {
			reasoning = strings.Join(reasoningSummaryParts, "\n\n")
		}
		hasReasoningMetadata := reasoning != "" || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != ""
		if hasReasoningMetadata {
			reasoningKind = core.NormalizeReasoningKind(reasoningKind)
		} else {
			reasoningKind = ""
		}
		reasoningTitle := ""
		if reasoningKind == core.ReasoningKindSummary {
			reasoningTitle = internalreasoning.ParseReasoningSummary(reasoning).Title
		}
		parts = append(parts, core.Part{
			Type:                      core.PartText,
			Text:                      text,
			ReasoningContent:          reasoning,
			ReasoningSummaryParts:     append([]string(nil), reasoningSummaryParts...),
			ReasoningItemID:           reasoningItemID,
			ReasoningEncryptedContent: reasoningEncryptedContent,
			ReasoningKind:             reasoningKind,
			ReasoningSummaryTitle:     reasoningTitle,
		})
	}
	for i := range toolCalls {
		call := toolCalls[i]
		parts = append(parts, core.Part{Type: core.PartToolCall, ToolCall: &call})
	}
	return core.Message{Role: core.RoleAssistant, Parts: parts}
}

func buildInterleavedAssistantMessageWithReasoningMetadata(orderedParts []core.Part, reasoning string, reasoningSummaryParts []string, reasoningItemID, reasoningEncryptedContent string, reasoningKind core.ReasoningKind) core.Message {
	parts := append([]core.Part(nil), orderedParts...)
	metadata := buildAssistantMessageWithReasoningMetadata("", nil, reasoning, reasoningSummaryParts, reasoningItemID, reasoningEncryptedContent, reasoningKind)
	if len(metadata.Parts) == 0 {
		return core.Message{Role: core.RoleAssistant, Parts: parts}
	}

	metadataPart := metadata.Parts[0]
	for i := range parts {
		if parts[i].Type != core.PartText {
			continue
		}
		parts[i].ReasoningContent = metadataPart.ReasoningContent
		parts[i].ReasoningSummaryParts = metadataPart.ReasoningSummaryParts
		parts[i].ReasoningItemID = metadataPart.ReasoningItemID
		parts[i].ReasoningEncryptedContent = metadataPart.ReasoningEncryptedContent
		parts[i].ReasoningKind = metadataPart.ReasoningKind
		parts[i].ReasoningSummaryTitle = metadataPart.ReasoningSummaryTitle
		return core.Message{Role: core.RoleAssistant, Parts: parts}
	}

	parts = append([]core.Part{metadataPart}, parts...)
	return core.Message{Role: core.RoleAssistant, Parts: parts}
}

// buildApprovalTranscript assembles the policy-review evidence a tool approval
// reviewer sees: review-only prefix (e.g. a parent agent's transcript), the
// durable conversation, the in-progress assistant turn that requested the tool,
// and any tool results already produced this turn. Guardian denies when it finds
// no real operator message here, so every tool-execution path must build it.
//
// results are appended after the assistant message rather than interleaved with
// the calls that produced them. Providers with an inline tool loop (grok-bin,
// cursor-bin) can therefore present the Nth review with one assistant message
// listing calls 1..N followed by results 1..N-1, which reads as a single planned
// batch rather than a call/result chain. All evidence is present and guardian
// stamps each entry with an index, but the causal link between a tool result and
// a command it induced is flattened. Restoring it needs per-call assistant
// segments, which the non-ordered path cannot currently reconstruct.
func buildApprovalTranscript(prefix, messages []core.Message, assistant core.Message, results ...core.Message) []core.Message {
	transcript := make([]core.Message, 0, len(prefix)+len(messages)+1+len(results))
	transcript = appendApprovalEvidence(transcript, prefix...)
	transcript = appendApprovalEvidence(transcript, messages...)
	if stripped := withoutProviderReplayParts(assistant); len(stripped.Parts) > 0 {
		transcript = append(transcript, stripped)
	}
	return appendApprovalEvidence(transcript, results...)
}

func appendApprovalEvidence(dst []core.Message, messages ...core.Message) []core.Message {
	for _, msg := range messages {
		dst = append(dst, withoutProviderReplayParts(msg))
	}
	return dst
}

// withoutProviderReplayParts drops opaque provider protocol state from a message
// destined for policy review. Replay parts carry encrypted/provider-private
// payloads that exist only to be echoed back to the provider; they are never
// rendered as review evidence and must not reach a reviewer. Stripping happens
// here rather than at each caller because durable history, a parent agent's
// prefix, and the in-progress assistant turn can all carry replay parts, and
// only this choke point sees every source. The returned message shares the
// original parts; the input is never mutated.
func withoutProviderReplayParts(msg core.Message) core.Message {
	replays := 0
	for _, part := range msg.Parts {
		if part.Type == core.PartProviderReplay {
			replays++
		}
	}
	if replays == 0 {
		return msg
	}
	parts := make([]core.Part, 0, len(msg.Parts)-replays)
	for _, part := range msg.Parts {
		if part.Type == core.PartProviderReplay {
			continue
		}
		parts = append(parts, part)
	}
	msg.Parts = parts
	return msg
}
