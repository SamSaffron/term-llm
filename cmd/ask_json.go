package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/ui"
)

// jsonEmitter serializes ui.StreamEvent and related metadata as JSONL
// events to an io.Writer. Every event is a single-line JSON object
// terminated by '\n'. Safe for concurrent use; seq/ts assignment and
// writes happen atomically under a single mutex so output order
// matches seq order.
type jsonEmitter struct {
	w   io.Writer
	mu  sync.Mutex
	seq int64
}

func newJSONEmitter(w io.Writer) *jsonEmitter {
	return &jsonEmitter{w: w}
}

// emit writes a single JSON event line. The payload map is merged with the
// envelope (type, seq, ts). Callers must not set "type", "seq", or "ts".
func (e *jsonEmitter) emit(eventType string, payload map[string]any) error {
	if e == nil {
		return nil
	}
	obj := make(map[string]any, len(payload)+3)
	for k, v := range payload {
		obj[k] = v
	}
	obj["type"] = eventType

	e.mu.Lock()
	defer e.mu.Unlock()

	obj["seq"] = e.seq
	e.seq++
	obj["ts"] = time.Now().UTC().Format(time.RFC3339Nano)

	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", eventType, err)
	}
	if _, err := e.w.Write(data); err != nil {
		return err
	}
	_, err = e.w.Write([]byte{'\n'})
	return err
}

// terminalFlushedError indicates that an error occurred during JSON
// streaming and the terminal events (error, stats, done) have already
// been emitted. Callers receiving this error MUST NOT call
// emitFatalError — doing so would duplicate the terminal record.
type terminalFlushedError struct {
	err error
}

func (t *terminalFlushedError) Error() string { return t.err.Error() }
func (t *terminalFlushedError) Unwrap() error { return t.err }

// isTerminalFlushed reports whether err indicates the terminal JSON
// record has already been written. Callers should use this to avoid
// re-emitting terminal events.
func isTerminalFlushed(err error) bool {
	var tfe *terminalFlushedError
	return errors.As(err, &tfe)
}

// sessionInfo is the static metadata emitted in the session.started event.
type sessionInfo struct {
	SessionID string
	Provider  string
	Model     string
	Agent     string
	Tools     string
	MCP       string
	Yolo      bool
	Search    bool
	Resuming  bool
}

// streamJSON consumes ui.StreamEvent values from events and emits JSONL
// events through e. It emits session.started first, a stats event and a
// done event last (in that order) even when the context is cancelled.
//
// If a ui.StreamEventError is observed, an error event is emitted and the
// wrapped error is returned after stats/done are flushed.
func streamJSON(ctx context.Context, events <-chan ui.StreamEvent, e *jsonEmitter, stats *ui.SessionStats, info sessionInfo) error {
	if err := emitSessionStarted(e, info); err != nil {
		return err
	}
	totalTokens, streamErr, writeErr := streamJSONEvents(ctx, events, e)
	if writeErr != nil {
		return writeErr
	}
	if err := emitFinal(e, stats, totalTokens); err != nil {
		return err
	}
	if streamErr != nil {
		return &terminalFlushedError{err: streamErr}
	}
	return nil
}

// emitSessionStarted writes the opening session.started event.
func emitSessionStarted(e *jsonEmitter, info sessionInfo) error {
	return e.emit("session.started", map[string]any{
		"session_id": info.SessionID,
		"provider":   info.Provider,
		"model":      info.Model,
		"agent":      stringOrNil(info.Agent),
		"tools":      info.Tools,
		"mcp":        info.MCP,
		"yolo":       info.Yolo,
		"search":     info.Search,
		"resuming":   info.Resuming,
	})
}

// streamJSONEvents pumps ui.StreamEvent values from events into the JSON
// emitter until the channel closes, a Done/Error event is seen, or the
// context is cancelled. The returned streamErr is the error reported by a
// ui.StreamEventError (nil otherwise); writeErr is a non-nil write failure.
func streamJSONEvents(ctx context.Context, events <-chan ui.StreamEvent, e *jsonEmitter) (totalTokens int, streamErr, writeErr error) {
	for {
		select {
		case <-ctx.Done():
			return totalTokens, streamErr, nil
		case ev, ok := <-events:
			if !ok {
				return totalTokens, streamErr, nil
			}
			if err := emitStreamEvent(e, ev); err != nil {
				return totalTokens, streamErr, err
			}
			switch ev.Type {
			case ui.StreamEventDone:
				return ev.Tokens, streamErr, nil
			case ui.StreamEventError:
				return totalTokens, ev.Err, nil
			}
		}
	}
}

// emitStreamEvent translates one ui.StreamEvent into a JSON event line.
// Events without a user-facing payload (e.g. interjection) are ignored.
func emitStreamEvent(e *jsonEmitter, ev ui.StreamEvent) error {
	switch ev.Type {
	case ui.StreamEventText:
		if ev.Text == "" {
			return nil
		}
		return e.emit("text.delta", map[string]any{"text": ev.Text})

	case ui.StreamEventToolStart:
		var args any
		if len(ev.ToolArgs) == 0 {
			args = nil
		} else {
			args = ev.ToolArgs
		}
		return e.emit("tool.started", map[string]any{
			"call_id": ev.ToolCallID,
			"name":    ev.ToolName,
			"info":    ev.ToolInfo,
			"args":    args,
		})

	case ui.StreamEventToolEnd:
		return e.emit("tool.completed", map[string]any{
			"call_id": ev.ToolCallID,
			"name":    ev.ToolName,
			"info":    ev.ToolInfo,
			"success": ev.ToolSuccess,
		})

	case ui.StreamEventUsage:
		return e.emit("usage", map[string]any{
			"input_tokens":        ev.InputTokens,
			"output_tokens":       ev.OutputTokens,
			"cached_input_tokens": ev.CachedTokens,
			"cache_write_tokens":  ev.WriteTokens,
		})

	case ui.StreamEventPhase:
		if ev.Phase == "" {
			return nil
		}
		return e.emit("phase", map[string]any{"phase": ev.Phase})

	case ui.StreamEventRetry:
		return e.emit("retry", map[string]any{
			"attempt":      ev.RetryAttempt,
			"max":          ev.RetryMax,
			"wait_seconds": ev.RetryWait,
		})

	case ui.StreamEventImage:
		if ev.ImagePath == "" {
			return nil
		}
		return e.emit("image", map[string]any{"path": ev.ImagePath})

	case ui.StreamEventDiff:
		return e.emit("diff", map[string]any{
			"path": ev.DiffPath,
			"old":  ev.DiffOld,
			"new":  ev.DiffNew,
			"line": ev.DiffLine,
		})

	case ui.StreamEventError:
		msg := ""
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		return e.emit("error", map[string]any{"message": msg})
	}
	return nil
}

// emitFinal writes the stats and done events at end-of-stream. Safe to
// call with a nil stats.
func emitFinal(e *jsonEmitter, stats *ui.SessionStats, totalTokens int) error {
	if stats != nil {
		stats.Finalize()
		duration := time.Since(stats.StartTime)
		if err := e.emit("stats", map[string]any{
			"duration_ms":         duration.Milliseconds(),
			"llm_ms":              stats.LLMTime.Milliseconds(),
			"tool_ms":             stats.ToolTime.Milliseconds(),
			"input_tokens":        stats.InputTokens,
			"output_tokens":       stats.OutputTokens,
			"cached_input_tokens": stats.CachedInputTokens,
			"cache_write_tokens":  stats.CacheWriteTokens,
			"tool_calls":          stats.ToolCallCount,
			"llm_calls":           stats.LLMCallCount,
		}); err != nil {
			return err
		}
		if totalTokens == 0 {
			totalTokens = stats.OutputTokens
		}
	}
	return e.emit("done", map[string]any{"tokens": totalTokens})
}

// emitProgressiveResult emits the progressive-run result as a dedicated
// event (only used in --json --progressive). Fields mirror progressiveRunResult.
func emitProgressiveResult(e *jsonEmitter, result progressiveRunResult) error {
	payload := map[string]any{
		"exit_reason": result.ExitReason,
		"finalized":   result.Finalized,
	}
	if result.SessionID != "" {
		payload["session_id"] = result.SessionID
	}
	if result.Sequence != 0 {
		payload["sequence"] = result.Sequence
	}
	if result.Reason != "" {
		payload["reason"] = result.Reason
	}
	if result.Message != "" {
		payload["message"] = result.Message
	}
	if len(result.Progress) > 0 {
		payload["progress"] = result.Progress
	}
	if result.FinalResponse != "" {
		payload["final_response"] = result.FinalResponse
	}
	if result.FallbackText != "" {
		payload["fallback_text"] = result.FallbackText
	}
	return e.emit("progressive.result", payload)
}

// emitFatalError emits a fatal error event, then stats/done so consumers
// always see a clean terminal record.
func emitFatalError(e *jsonEmitter, stats *ui.SessionStats, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		_ = e.emit("error", map[string]any{"message": "canceled"})
	} else {
		_ = e.emit("error", map[string]any{"message": err.Error()})
	}
	return emitFinal(e, stats, 0)
}

func stringOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
