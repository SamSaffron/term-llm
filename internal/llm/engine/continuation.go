package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"

	"github.com/samsaffron/term-llm/internal/restart"
)

type SuspendedError struct{ Continuation *core.Continuation }

func (*SuspendedError) Error() string { return "invocation suspended for process replacement" }

func (e *Engine) suspend(req core.Request, turn int, pending []core.ToolCall, interrupted bool, baseMessageCount int, metrics ...core.TurnMetrics) error {
	req.Resume = nil
	state := &core.Continuation{BaseMessageCount: baseMessageCount, Request: req, Turn: turn, Pending: pending, Interrupted: interrupted}
	if len(metrics) > 0 {
		state.PendingMetrics = metrics[0]
	}
	if exporter, ok := e.provider.(core.ProviderStateExporter); ok {
		state.ProviderState, _ = exporter.ExportProviderState()
	}
	// Freeze a private, serializable copy before relinquishing the producer. A
	// serialization failure is an ordinary failure, never a usable checkpoint.
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("serialize engine continuation: %w", err)
	}
	var frozen core.Continuation
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return err
	}
	return &SuspendedError{Continuation: &frozen}
}

func (e *Engine) waitActualTools() {
	e.callbackMu.RLock()
	done := e.steeringToolsSettled
	e.callbackMu.RUnlock()
	if done != nil {
		<-done
	}
}

// resumePending executes only the still-undispatched suffix. The assistant that
// requested these calls is already in the checkpoint and must not be re-emitted
// or appended a second time.
func (e *Engine) resumePending(ctx context.Context, req *core.Request, resume *core.Continuation, send corestream.Sender, callback TurnCompletedCallback) error {
	if len(resume.Pending) == 0 {
		return nil
	}
	calls := append([]core.ToolCall(nil), resume.Pending...)
	names := make(map[string]string)
	for i := range calls {
		if mapped := req.ToolMap[calls[i].Name]; mapped != "" {
			names[calls[i].ID] = calls[i].Name
			calls[i].Name = mapped
		}
		if _, ok := e.tools.Get(calls[i].Name); !ok {
			return fmt.Errorf("continuation tool %q is no longer available", calls[i].Name)
		}
	}
	for _, call := range calls {
		if err := send.Send(core.Event{Type: core.EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolArgs: call.Arguments}); err != nil {
			return err
		}
	}
	results, err := e.executeToolCalls(ctx, calls, req.ParallelToolCalls, send, req.Debug, req.DebugRaw, buildApprovalTranscript(req.ApprovalTranscriptPrefix, req.Messages, core.Message{}))
	if err != nil {
		return err
	}
	for i := range results {
		for j := range results[i].Parts {
			result := results[i].Parts[j].ToolResult
			if result != nil && names[result.ID] != "" {
				result.Name = names[result.ID]
			}
		}
	}
	req.Messages = append(req.Messages, results...)
	if callback != nil {
		persistCtx, cancel := callbackContext(ctx)
		metrics := resume.PendingMetrics
		metrics.ToolCalls = len(calls)
		err = callback(persistCtx, resume.Turn, results, metrics)
		cancel()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func interruptedForRestart(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), restart.ErrInterrupt)
}
