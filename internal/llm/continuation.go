package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/restart"
)

// Continuation is an engine execution boundary, not a new user request. Messages
// contain completed history; Pending contains only calls not yet dispatched.
// Tool results in Messages are never fed back through the tool executor.
type Continuation struct {
	PendingMetrics   TurnMetrics
	BaseMessageCount int
	DiscardPartial   bool
	Request          Request
	Turn             int
	Pending          []ToolCall
	ProviderState    []byte
	Interrupted      bool
}

type SuspendedError struct{ Continuation *Continuation }

func (*SuspendedError) Error() string { return "invocation suspended for process replacement" }

func (e *Engine) suspend(req Request, turn int, pending []ToolCall, interrupted bool, baseMessageCount int, metrics ...TurnMetrics) error {
	req.Resume = nil
	state := &Continuation{BaseMessageCount: baseMessageCount, Request: req, Turn: turn, Pending: pending, Interrupted: interrupted}
	if len(metrics) > 0 {
		state.PendingMetrics = metrics[0]
	}
	if exporter, ok := e.provider.(ProviderStateExporter); ok {
		state.ProviderState, _ = exporter.ExportProviderState()
	}
	// Freeze a private, serializable copy before relinquishing the producer. A
	// serialization failure is an ordinary failure, never a usable checkpoint.
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("serialize engine continuation: %w", err)
	}
	var frozen Continuation
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
func (e *Engine) resumePending(ctx context.Context, req *Request, resume *Continuation, send eventSender, callback TurnCompletedCallback) error {
	if len(resume.Pending) == 0 {
		return nil
	}
	calls := append([]ToolCall(nil), resume.Pending...)
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
		if err := send.Send(Event{Type: EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolArgs: call.Arguments}); err != nil {
			return err
		}
	}
	results, err := e.executeToolCalls(ctx, calls, req.ParallelToolCalls, send, req.Debug, req.DebugRaw, buildApprovalTranscript(req.ApprovalTranscriptPrefix, req.Messages, Message{}))
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

// CommittedText is the completed assistant prefix of this invocation, excluding
// history from earlier user turns and any discarded partial provider attempt.
func (c *Continuation) CommittedText() string {
	if c == nil {
		return ""
	}
	var text strings.Builder
	for _, message := range c.Request.Messages[min(max(c.BaseMessageCount, 0), len(c.Request.Messages)):] {
		if message.Role == RoleAssistant {
			text.WriteString(MessageText(message))
		}
	}
	return text.String()
}
