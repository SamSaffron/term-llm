package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/restart"
)

// toolCallOutcome is the provider-neutral result of one engine-owned tool
// execution. Both API providers and synchronous CLI bridges adapt this same
// value to their respective result protocols.
type toolCallOutcome struct {
	call   core.ToolCall
	output core.ToolOutput
	err    error
}

func (o toolCallOutcome) message() core.Message {
	if o.err != nil {
		return toolErrorMessageWithGuardian(o.call.ID, o.call.Name, fmt.Sprintf("Error: %v", o.err), o.call.ThoughtSig, o.output.GuardianReviews)
	}
	message := core.ToolResultMessageFromOutput(o.call.ID, o.call.Name, o.output, o.call.ThoughtSig)
	if role, text, ok := core.ApprovalTurnForTool(o.call.Name, o.output); ok {
		message.ApprovalRole = role
		message.ApprovalText = text
	}
	return message
}

// executeToolCallOutcomes executes one model-authored batch. Outcomes retain
// dispatch order even when workers finish out of order. Cancellation returns
// promptly with a terminal outcome for every announced call.
func (e *Engine) executeToolCallOutcomes(ctx context.Context, calls []core.ToolCall, parallel bool, send corestream.Sender, debug bool, debugRaw bool, transcript []core.Message) ([]toolCallOutcome, error) {
	outcomes := make([]toolCallOutcome, len(calls))
	if err := ctx.Err(); err != nil {
		for i, call := range calls {
			outcomes[i] = toolCallOutcome{call: call, err: err}
		}
		return outcomes, nil
	}
	if len(calls) == 0 {
		return outcomes, nil
	}

	toolCtx := core.ContextWithApprovalTranscript(ctx, transcript)
	if len(calls) == 1 || !parallel {
		for i, call := range calls {
			if err := ctx.Err(); err != nil {
				for j := i; j < len(calls); j++ {
					outcomes[j] = toolCallOutcome{call: calls[j], err: err}
				}
				break
			}
			outcomes[i] = e.executeSingleToolCallOutcomeSafe(toolCtx, call, send, debug, debugRaw)
		}
		return outcomes, nil
	}

	type indexedOutcome struct {
		index   int
		outcome toolCallOutcome
	}
	resultChan := make(chan indexedOutcome, len(calls))
	workerCount := maxParallelToolWorkers(len(calls))
	var nextCall atomic.Uint32
	for worker := 0; worker < workerCount; worker++ {
		workerCtx, release, err := restart.Child(toolCtx)
		if err != nil {
			return nil, err
		}
		go func() {
			defer release()
			for {
				if ctx.Err() != nil {
					return
				}
				idx := int(nextCall.Add(1)) - 1
				if idx >= len(calls) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				resultChan <- indexedOutcome{index: idx, outcome: e.executeSingleToolCallOutcomeSafe(workerCtx, calls[idx], send, debug, debugRaw)}
			}
		}()
	}

	for remaining := len(calls); remaining > 0; remaining-- {
		select {
		case result := <-resultChan:
			outcomes[result.index] = result.outcome
		case <-ctx.Done():
			for drained := false; !drained; {
				select {
				case result := <-resultChan:
					outcomes[result.index] = result.outcome
				default:
					drained = true
				}
			}
			for i := range outcomes {
				if outcomes[i].call.ID == "" {
					outcomes[i] = toolCallOutcome{call: calls[i], err: ctx.Err()}
				}
			}
			return outcomes, nil
		}
	}
	return outcomes, nil
}

// executeToolCalls executes multiple tool calls, potentially in parallel.
func (e *Engine) executeToolCalls(ctx context.Context, calls []core.ToolCall, parallel bool, send corestream.Sender, debug bool, debugRaw bool, transcriptOpt ...[]core.Message) ([]core.Message, error) {
	var transcript []core.Message
	if len(transcriptOpt) > 0 {
		transcript = transcriptOpt[0]
	}
	outcomes, err := e.executeToolCallOutcomes(ctx, calls, parallel, send, debug, debugRaw, transcript)
	if err != nil {
		return nil, err
	}
	messages := make([]core.Message, len(outcomes))
	for i, outcome := range outcomes {
		messages[i] = outcome.message()
	}
	return messages, nil
}

// executeSingleToolCallOutcomeSafe wraps execution with panic recovery.
func (e *Engine) executeSingleToolCallOutcomeSafe(ctx context.Context, call core.ToolCall, send corestream.Sender, debug bool, debugRaw bool) (outcome toolCallOutcome) {
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("tool panicked: %v", r)
			sendToolExecEnd(send, core.Event{Type: core.EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolSuccess: false})
			outcome = toolCallOutcome{call: call, err: err}
		}
	}()
	return e.executeSingleToolCallOutcome(ctx, call, send, debug, debugRaw)
}

// executeSingleToolCallSafe is retained as the message adapter used by callers
// that execute one call outside a complete batch.
func (e *Engine) executeSingleToolCallSafe(ctx context.Context, call core.ToolCall, send corestream.Sender, debug bool, debugRaw bool) ([]core.Message, error) {
	outcome := e.executeSingleToolCallOutcomeSafe(ctx, call, send, debug, debugRaw)
	return []core.Message{outcome.message()}, nil
}

type toolExecutionResult struct {
	output     core.ToolOutput
	err        error
	panicValue any
}

// executeToolWithCancellation isolates Tool.Execute so a tool that ignores its
// context cannot keep the engine blocked after the caller cancels. The buffered
// result channel lets an abandoned invocation finish without blocking later.
func (e *Engine) executeToolWithCancellation(ctx context.Context, tool core.Tool, args json.RawMessage) (core.ToolOutput, error, any) {
	ctx, release, err := restart.Child(ctx)
	if err != nil {
		return core.ToolOutput{}, err, nil
	}
	if err := e.beginSteeringTool(ctx); err != nil {
		release()
		return core.ToolOutput{}, err, nil
	}

	results := make(chan toolExecutionResult, 1)
	go func() {
		defer release()
		defer e.actualSteeringToolDone()
		result := toolExecutionResult{}
		defer func() {
			if r := recover(); r != nil {
				result.panicValue = r
			}
			results <- result
		}()
		result.output, result.err = tool.Execute(ctx, args)
	}()

	select {
	case result := <-results:
		return result.output, result.err, result.panicValue
	case <-ctx.Done():
		return core.ToolOutput{}, ctx.Err(), nil
	}
}

// startToolHeartbeat emits heartbeat events only if a tool is still running
// after toolHeartbeatInterval. Most tools complete quickly, so using AfterFunc
// avoids starting a goroutine and ticker on every tool invocation.
func startToolHeartbeat(ctx context.Context, callID, toolName string, send corestream.Sender) func() {
	if send.Chan() == nil {
		return func() {}
	}

	var stopped atomic.Bool
	heartbeat := core.Event{Type: core.EventHeartbeat, ToolCallID: callID, ToolName: toolName}

	timer := time.AfterFunc(toolHeartbeatInterval, func() {
		ticker := time.NewTicker(toolHeartbeatInterval)
		defer ticker.Stop()
		for {
			if stopped.Load() {
				return
			}
			send.TrySend(heartbeat)
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	})

	return func() {
		stopped.Store(true)
		timer.Stop()
	}
}

func toolErrorMessageWithGuardian(id, name, text string, thoughtSig []byte, reviews []core.GuardianReview) core.Message {
	message := core.ToolErrorMessage(id, name, text, thoughtSig)
	if len(message.Parts) > 0 && message.Parts[0].ToolResult != nil {
		message.Parts[0].ToolResult.GuardianReviews = append([]core.GuardianReview(nil), reviews...)
	}
	return message
}

const (
	reliableSpawnAgentTerminalName  = "spawn_agent"
	reliableWaitForJobsTerminalName = "wait_for_jobs"
)

// sendToolExecEnd keeps delegation completion lossless. A child session or
// queued run can become terminal before the parent tool result is durable, so
// dropping either event leaves Web clients with an in-flight placeholder and
// an unreleased response timer hold. Other tool ends remain best-effort to
// preserve the existing worker backpressure behavior.
func sendToolExecEnd(send corestream.Sender, event core.Event) {
	switch event.ToolName {
	case reliableSpawnAgentTerminalName, reliableWaitForJobsTerminalName:
		_ = send.Send(event)
	default:
		send.TrySend(event)
	}
}

// executeSingleToolCall is the historical message adapter used by focused
// tests and non-batch callers.
func (e *Engine) executeSingleToolCall(ctx context.Context, call core.ToolCall, send corestream.Sender, debug bool, debugRaw bool) ([]core.Message, error) {
	outcome := e.executeSingleToolCallOutcome(ctx, call, send, debug, debugRaw)
	return []core.Message{outcome.message()}, nil
}

// executeSingleToolCallOutcome executes a single tool call and retains its
// structured output for both transcript and bridge result adapters.
func (e *Engine) executeSingleToolCallOutcome(ctx context.Context, call core.ToolCall, send corestream.Sender, debug bool, debugRaw bool) toolCallOutcome {
	tool, ok := e.tools.Get(call.Name)
	if !ok {
		// suggest_commands is a structured-output passthrough used by exec mode;
		// observing the call arguments is the execution, so no registry tool runs.
		if call.Name == core.SuggestCommandsToolName {
			output := core.TextOutput("OK")
			env.debugToolResult(debug, call.ID, call.Name, output.Content)
			sendToolExecEnd(send, core.Event{Type: core.EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: true})
			return toolCallOutcome{call: call, output: output}
		}
		// A declared client tool answered while an inline provider waits (see
		// ClientToolRunner) runs like a registered tool, minus the allow-list:
		// passthrough never applied it, as the client runs its own tools.
		if tool = core.ClientTool(ctx, call.Name); tool == nil {
			errMsg := fmt.Sprintf("Error: tool not registered: %s", call.Name)
			env.debugToolResult(debug, call.ID, call.Name, errMsg)
			sendToolExecEnd(send, core.Event{Type: core.EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false})
			return toolCallOutcome{call: call, err: fmt.Errorf("tool not registered: %s", call.Name)}
		}
	}

	if ok && !e.toolCallAllowed(ctx, call.Name) {
		errMsg := fmt.Sprintf("Error: tool '%s' is not in the active skill's allowed-tools list", call.Name)
		env.debugToolResult(debug, call.ID, call.Name, errMsg)
		sendToolExecEnd(send, core.Event{Type: core.EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false})
		return toolCallOutcome{call: call, err: fmt.Errorf("tool '%s' is not in the active skill's allowed-tools list", call.Name)}
	}

	// Add call ID to context for spawn_agent event bubbling
	toolCtx := core.ContextWithCallID(ctx, call.ID)
	toolCtx, guardianReviews := core.ContextWithGuardianReviewCapture(toolCtx)

	stopHeartbeat := startToolHeartbeat(ctx, call.ID, call.Name, send)
	defer stopHeartbeat()

	output, err, panicValue := e.executeToolWithCancellation(toolCtx, tool, call.Arguments)
	output.GuardianReviews = guardianReviews()
	if planner := e.currentToolPlanner(); planner != nil {
		planner.ToolExecuted(core.SessionIDFromContext(ctx), core.ToolRunIDFromContext(ctx), call.Name)
	}
	if panicValue != nil {
		panic(panicValue)
	}
	info := e.getToolPreview(call)

	// Truncate large tool outputs (global limit, then compaction limit).
	if err == nil {
		output = e.applyToolOutputTruncation(output)
	}

	if err != nil {
		errMsg := fmt.Sprintf("Error: %v", err)
		env.debugToolResult(debug, call.ID, call.Name, errMsg)
		sendToolExecEnd(send, core.Event{Type: core.EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolSuccess: false})
		return toolCallOutcome{call: call, output: output, err: err}
	}

	env.debugToolResult(debug, call.ID, call.Name, output.Content)
	env.debugRawToolResult(debugRaw, call.ID, call.Name, output.Content)
	// Spawn-agent completion is reliable because its durable parent result may be
	// delayed behind other parallel tools. Other tool ends remain best-effort.
	sendToolExecEnd(send, core.Event{
		Type:                       core.EventToolExecEnd,
		ToolCallID:                 call.ID,
		ToolName:                   call.Name,
		ToolInfo:                   info,
		ToolSuccess:                !output.TimedOut && !output.IsError,
		ToolOutput:                 output.Content,
		ToolDiffs:                  output.Diffs,
		ToolFileChanges:            output.FileChanges,
		ToolFilesystemObservations: output.FilesystemObservations,
		ToolOutputClaimDiagnostics: output.OutputClaimDiagnostics,
		ToolImages:                 output.Images,
		ToolMedia:                  append([]core.MediaArtifact(nil), output.Media...),
	})
	return toolCallOutcome{call: call, output: output}
}

// toolCallAllowed applies ordinary execution policy. A planner may separately
// authorise only its exact control tool for this active run; this does not
// change the public IsToolAllowed result or grant authority to any
// self-asserted tool.
func (e *Engine) toolCallAllowed(ctx context.Context, name string) bool {
	if e.IsToolAllowed(name) {
		return true
	}
	planner := e.currentToolPlanner()
	return planner != nil && planner.AllowsPlannerTool(core.ToolRunIDFromContext(ctx), name)
}

func (e *Engine) withToolPreview(calls []core.ToolCall) []core.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	withPreview := make([]core.ToolCall, len(calls))
	for i := range calls {
		withPreview[i] = calls[i]
		withPreview[i].ToolInfo = e.getToolPreview(withPreview[i])
	}
	return withPreview
}

func ensureToolCallIDs(calls []core.ToolCall) []core.ToolCall {
	for i := range calls {
		if strings.TrimSpace(calls[i].ID) == "" {
			calls[i].ID = core.NewSyntheticToolCallID()
		}
	}
	return calls
}

func dedupeToolCalls(calls []core.ToolCall) []core.ToolCall {
	if len(calls) < 2 {
		return calls
	}
	seen := make(map[string]struct{}, len(calls))
	out := make([]core.ToolCall, 0, len(calls))
	for _, call := range calls {
		id := strings.TrimSpace(call.ID)
		if id == "" {
			out = append(out, call)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, call)
	}
	return out
}

// getToolPreview returns a preview string for a tool call.
func (e *Engine) getToolPreview(call core.ToolCall) string {
	if call.ToolInfo != "" {
		return call.ToolInfo
	}
	if tool, ok := e.tools.Get(call.Name); ok {
		if preview := tool.Preview(call.Arguments); preview != "" {
			if !strings.HasPrefix(preview, "(") {
				return "(" + preview + ")"
			}
			return preview
		}
	}
	return ExtractToolInfo(call)
}

func formatToolArgs(args map[string]any, maxLen, maxParams int) string {
	if len(args) == 0 {
		return ""
	}

	type argPair struct {
		key string
		val string
	}
	var pairs []argPair

	for k, v := range args {
		var valStr string
		switch val := v.(type) {
		case string:
			if val == "" {
				continue
			}
			valStr = val
		case float64:
			if val == float64(int(val)) {
				valStr = fmt.Sprintf("%d", int(val))
			} else {
				valStr = fmt.Sprintf("%g", val)
			}
		case bool:
			valStr = fmt.Sprintf("%v", val)
		default:
			continue
		}

		if len(valStr) > 200 {
			valStr = valStr[:197] + "..."
		}
		pairs = append(pairs, argPair{key: k, val: valStr})
	}

	if len(pairs) == 0 {
		return ""
	}

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].key < pairs[j].key
	})

	var result string
	if len(pairs) == 1 {
		result = "(" + pairs[0].val + ")"
	} else {
		var parts []string
		for i, p := range pairs {
			if i >= maxParams {
				parts = append(parts, "...")
				break
			}
			parts = append(parts, p.key+":"+p.val)
		}
		result = "(" + strings.Join(parts, ", ") + ")"
	}

	if len(result) > maxLen {
		result = result[:maxLen-4] + "...)"
	}

	return result
}

// ExtractToolInfo extracts a preview string from tool call arguments.
// Used for displaying tool calls in the UI (e.g., "(path:main.go)" for read_file).
func ExtractToolInfo(call core.ToolCall) string {
	if len(call.Arguments) == 0 {
		return ""
	}

	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return ""
	}

	return formatToolArgs(args, 500, 5)
}
