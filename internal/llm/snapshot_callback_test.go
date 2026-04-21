package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectSnapshots returns a helper that records every snapshot callback invocation
// under a mutex and exposes safe accessors for tests.
type snapshotRecorder struct {
	mu       sync.Mutex
	calls    []Message
	err      error
	onInvoke func()
}

func (r *snapshotRecorder) callback() AssistantSnapshotCallback {
	return func(ctx context.Context, _ int, msg Message) error {
		r.mu.Lock()
		r.calls = append(r.calls, msg)
		r.mu.Unlock()
		if r.onInvoke != nil {
			r.onInvoke()
		}
		return r.err
	}
}

func (r *snapshotRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *snapshotRecorder) at(i int) Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

// drainStreamErr drains the stream and returns any terminal error event err.
func drainStreamErr(t *testing.T, stream Stream) error {
	t.Helper()
	var streamErr error
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return streamErr
		}
		if err != nil {
			return err
		}
		if event.Type == EventError && event.Err != nil {
			streamErr = event.Err
		}
	}
}

// TestSnapshotFiresOnToolCall verifies the snapshot callback is invoked exactly
// once when the provider streams a single async tool call.
func TestSnapshotFiresOnToolCall(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventTextDelta, Text: "working"},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{{Type: EventTextDelta, Text: "done"}, {Type: EventDone}}
			}
		},
	}

	engine := NewEngine(provider, registry)
	rec := &snapshotRecorder{}
	engine.SetAssistantSnapshotCallback(rec.callback())

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	if err := drainStreamErr(t, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if rec.count() != 1 {
		t.Fatalf("snapshot calls = %d, want 1", rec.count())
	}
	msg := rec.at(0)
	if msg.Role != RoleAssistant {
		t.Errorf("snapshot role = %v, want assistant", msg.Role)
	}
	var foundText, foundCall bool
	for _, p := range msg.Parts {
		if p.Type == PartText && p.Text == "working" {
			foundText = true
		}
		if p.Type == PartToolCall && p.ToolCall != nil && p.ToolCall.ID == "call-1" {
			foundCall = true
		}
	}
	if !foundText {
		t.Errorf("snapshot missing text part, got parts %+v", msg.Parts)
	}
	if !foundCall {
		t.Errorf("snapshot missing tool call part, got parts %+v", msg.Parts)
	}
}

// TestSnapshotFiresCumulativelyPerToolCall verifies the snapshot fires once per
// EventToolCall and each invocation contains all tool calls accumulated so far.
func TestSnapshotFiresCumulativelyPerToolCall(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventTextDelta, Text: "step "},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-A", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-B", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{{Type: EventTextDelta, Text: "done"}, {Type: EventDone}}
			}
		},
	}

	engine := NewEngine(provider, registry)
	rec := &snapshotRecorder{}
	engine.SetAssistantSnapshotCallback(rec.callback())

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	if err := drainStreamErr(t, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if rec.count() != 2 {
		t.Fatalf("snapshot calls = %d, want 2", rec.count())
	}

	// First snapshot: 1 tool call (call-A).
	first := rec.at(0)
	callIDs := extractCallIDs(first)
	if len(callIDs) != 1 || callIDs[0] != "call-A" {
		t.Errorf("snapshot #1 tool call IDs = %v, want [call-A]", callIDs)
	}

	// Second snapshot: 2 tool calls (call-A, call-B) — cumulative.
	second := rec.at(1)
	callIDs = extractCallIDs(second)
	if len(callIDs) != 2 || callIDs[0] != "call-A" || callIDs[1] != "call-B" {
		t.Errorf("snapshot #2 tool call IDs = %v, want [call-A call-B]", callIDs)
	}
}

// TestSnapshotFiresBeforeSendFailure verifies that snapshot is fired before
// send.Send is attempted. This tests the root cause of PR #426: when the
// consumer cancels the context, send.Send fails, but the snapshot callback has
// already persisted the partial assistant state.
//
// We trigger this by using a slow snapshot callback that cancels the parent
// context from within itself — guaranteeing that by the time send.Send runs,
// the context is already done.
func TestSnapshotFiresBeforeSendFailure(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventTextDelta, Text: "prelude"},
				{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
				{Type: EventDone},
			}
		},
	}

	engine := NewEngine(provider, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &snapshotRecorder{}
	// Cancel context from within the snapshot callback so that the subsequent
	// send.Send for the same EventToolCall races with cancellation. Even under
	// worst-case ordering, the snapshot must have already been recorded.
	rec.onInvoke = func() {
		cancel()
	}
	engine.SetAssistantSnapshotCallback(rec.callback())

	stream, err := engine.Stream(ctx, Request{
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	// Drain until EOF or error — stream will terminate because ctx was cancelled.
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	stream.Close()

	if rec.count() != 1 {
		t.Fatalf("snapshot calls = %d, want 1 (fired before send.Send)", rec.count())
	}
	msg := rec.at(0)
	callIDs := extractCallIDs(msg)
	if len(callIDs) != 1 || callIDs[0] != "call-1" {
		t.Errorf("snapshot tool call IDs = %v, want [call-1]", callIDs)
	}
}

// TestSnapshotIncludesReasoningMetadata verifies the snapshot captures reasoning
// text, reasoning item ID, and encrypted reasoning content alongside tool calls.
func TestSnapshotIncludesReasoningMetadata(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventReasoningDelta, Text: "thinking", ReasoningItemID: "reasoning-1", ReasoningEncryptedContent: "encrypted-blob"},
					{Type: EventTextDelta, Text: "visible"},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{{Type: EventTextDelta, Text: "done"}, {Type: EventDone}}
			}
		},
	}

	engine := NewEngine(provider, registry)
	rec := &snapshotRecorder{}
	engine.SetAssistantSnapshotCallback(rec.callback())

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	if err := drainStreamErr(t, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if rec.count() != 1 {
		t.Fatalf("snapshot calls = %d, want 1", rec.count())
	}
	msg := rec.at(0)
	var textPart *Part
	for i := range msg.Parts {
		if msg.Parts[i].Type == PartText {
			textPart = &msg.Parts[i]
			break
		}
	}
	if textPart == nil {
		t.Fatalf("snapshot missing text part, parts = %+v", msg.Parts)
	}
	if textPart.Text != "visible" {
		t.Errorf("snapshot text = %q, want %q", textPart.Text, "visible")
	}
	if textPart.ReasoningContent != "thinking" {
		t.Errorf("snapshot reasoning = %q, want %q", textPart.ReasoningContent, "thinking")
	}
	if textPart.ReasoningItemID != "reasoning-1" {
		t.Errorf("snapshot reasoning ID = %q, want %q", textPart.ReasoningItemID, "reasoning-1")
	}
	if textPart.ReasoningEncryptedContent != "encrypted-blob" {
		t.Errorf("snapshot encrypted reasoning = %q, want %q", textPart.ReasoningEncryptedContent, "encrypted-blob")
	}
}

// syncSnapshotProvider emits a single synchronous tool call (claude-bin MCP
// pattern) and waits for the engine's tool execution result on the response
// channel.
type syncSnapshotProvider struct{}

func (p *syncSnapshotProvider) Name() string       { return "sync-snapshot-fake" }
func (p *syncSnapshotProvider) Credential() string { return "test" }
func (p *syncSnapshotProvider) Capabilities() Capabilities {
	return Capabilities{ToolCalls: true}
}

func (p *syncSnapshotProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		responseCh := make(chan ToolExecutionResponse, 1)
		if err := send.Send(Event{Type: EventTextDelta, Text: "sync"}); err != nil {
			return err
		}
		if err := send.Send(Event{
			Type:         EventToolCall,
			ToolCallID:   "sync-call-1",
			ToolName:     "count_tool",
			Tool:         &ToolCall{ID: "sync-call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)},
			ToolResponse: responseCh,
		}); err != nil {
			return err
		}
		select {
		case <-responseCh:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), nil
}

// TestSnapshotFiresOnSyncToolCall verifies the snapshot fires for sync tool
// calls (claude-bin MCP path where ToolResponse is non-nil).
func TestSnapshotFiresOnSyncToolCall(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	engine := NewEngine(&syncSnapshotProvider{}, registry)
	rec := &snapshotRecorder{}
	engine.SetAssistantSnapshotCallback(rec.callback())

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()
	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out draining stream")
	}

	if rec.count() < 1 {
		t.Fatalf("snapshot calls = %d, want >= 1", rec.count())
	}
	first := rec.at(0)
	callIDs := extractCallIDs(first)
	if len(callIDs) != 1 || callIDs[0] != "sync-call-1" {
		t.Errorf("snapshot tool call IDs = %v, want [sync-call-1]", callIDs)
	}
}

// TestSnapshotNotFiredOnPureTextTurn verifies the snapshot callback is NOT
// invoked when the provider streams only text (no tool calls). Persistence in
// that case is handled by the turn callback.
func TestSnapshotNotFiredOnPureTextTurn(t *testing.T) {
	registry := NewToolRegistry()

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventTextDelta, Text: "just text"},
				{Type: EventDone},
			}
		},
	}

	engine := NewEngine(provider, registry)
	rec := &snapshotRecorder{}
	engine.SetAssistantSnapshotCallback(rec.callback())

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	if err := drainStreamErr(t, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if rec.count() != 0 {
		t.Fatalf("snapshot calls = %d, want 0 for pure-text turn", rec.count())
	}
}

// TestSnapshotNotFiredOnToolChoiceRetry verifies that when the first turn
// returns no tool calls and tool choice is re-broadened for a retry, no
// snapshots are fired on that empty first attempt.
func TestSnapshotNotFiredOnToolChoiceRetry(t *testing.T) {
	tool := &namedTool{name: "forced_tool"}
	registry := NewToolRegistry()
	registry.Register(tool)

	// First attempt: empty stream (no tool call). Second attempt (after
	// ToolChoice is restored to auto): emits a tool call.
	callIdx := 0
	var mu sync.Mutex
	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			mu.Lock()
			idx := callIdx
			callIdx++
			mu.Unlock()
			switch idx {
			case 0:
				return []Event{{Type: EventTextDelta, Text: "retry"}, {Type: EventDone}}
			case 1:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "retry-call", Name: "forced_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{{Type: EventTextDelta, Text: "done"}, {Type: EventDone}}
			}
		},
	}

	engine := NewEngine(provider, registry)
	rec := &snapshotRecorder{}
	engine.SetAssistantSnapshotCallback(rec.callback())

	stream, err := engine.Stream(context.Background(), Request{
		Messages:   []Message{UserText("test")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceName, Name: "forced_tool"},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	if err := drainStreamErr(t, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	// We expect exactly one snapshot — for the tool call on the retry (second provider attempt).
	if rec.count() != 1 {
		t.Fatalf("snapshot calls = %d, want 1 (only retry attempt had a tool call)", rec.count())
	}
	callIDs := extractCallIDs(rec.at(0))
	if len(callIDs) != 1 || callIDs[0] != "retry-call" {
		t.Errorf("snapshot tool call IDs = %v, want [retry-call]", callIDs)
	}
}

// TestSnapshotFiresBeforeStreamErrorAfterToolCall verifies snapshot ordering when
// a provider Recv error follows a successfully-emitted tool call.
func TestSnapshotFiresBeforeStreamErrorAfterToolCall(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &streamProvider{
		stream: &errAfterEventsStream{
			events: []Event{
				{Type: EventTextDelta, Text: "before-err"},
				{Type: EventToolCall, Tool: &ToolCall{ID: "pre-err-call", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
			},
			err: errors.New("stream recv failed"),
		},
	}

	engine := NewEngine(provider, registry)
	rec := &snapshotRecorder{}
	engine.SetAssistantSnapshotCallback(rec.callback())

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	streamErr := drainStreamErr(t, stream)
	if streamErr == nil || !strings.Contains(streamErr.Error(), "stream recv failed") {
		t.Fatalf("expected stream error containing 'stream recv failed', got %v", streamErr)
	}

	if rec.count() != 1 {
		t.Fatalf("snapshot calls = %d, want 1 (fired for the tool call before recv error)", rec.count())
	}
	callIDs := extractCallIDs(rec.at(0))
	if len(callIDs) != 1 || callIDs[0] != "pre-err-call" {
		t.Errorf("snapshot tool call IDs = %v, want [pre-err-call]", callIDs)
	}
}

// extractCallIDs returns the ordered IDs of every PartToolCall part in the
// given message.
func extractCallIDs(msg Message) []string {
	var ids []string
	for _, p := range msg.Parts {
		if p.Type == PartToolCall && p.ToolCall != nil {
			ids = append(ids, p.ToolCall.ID)
		}
	}
	return ids
}
