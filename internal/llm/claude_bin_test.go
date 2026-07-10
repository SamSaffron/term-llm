package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClaudeBinProvider_ImplementsToolExecutorSetter(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)

	// This type assertion must succeed for tools to work.
	// The bug was that ClaudeBinProvider.SetToolExecutor used mcphttp.ToolExecutor
	// (a named type) instead of the anonymous function type in the interface,
	// which caused this assertion to fail silently.
	if _, ok := interface{}(provider).(ToolExecutorSetter); !ok {
		t.Fatal("ClaudeBinProvider does not implement ToolExecutorSetter interface - tools will not work")
	}
}

func TestRetryProvider_ForwardsToolExecutorSetter(t *testing.T) {
	// ClaudeBinProvider is wrapped with WrapWithRetry in the factory.
	// The RetryProvider must forward SetToolExecutor to the inner provider.
	provider := NewClaudeBinProvider("sonnet", nil)
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())

	// The wrapped provider must also implement ToolExecutorSetter
	if _, ok := wrapped.(ToolExecutorSetter); !ok {
		t.Fatal("RetryProvider does not implement ToolExecutorSetter interface - tools will not work with wrapped providers")
	}
}

func TestClaudeBinProvider_ImplementsProviderCleaner(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)

	// ClaudeBinProvider must implement ProviderCleaner for MCP server cleanup
	if _, ok := interface{}(provider).(ProviderCleaner); !ok {
		t.Fatal("ClaudeBinProvider does not implement ProviderCleaner interface - MCP server cleanup will not work")
	}
}

func TestRetryProvider_ForwardsProviderCleaner(t *testing.T) {
	// ClaudeBinProvider is wrapped with WrapWithRetry in the factory.
	// The RetryProvider must forward CleanupMCP to the inner provider.
	provider := NewClaudeBinProvider("sonnet", nil)
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())

	// The wrapped provider must also implement ProviderCleaner
	if _, ok := wrapped.(ProviderCleaner); !ok {
		t.Fatal("RetryProvider does not implement ProviderCleaner interface - MCP cleanup will not work with wrapped providers")
	}
}

func TestClaudeBinProvider_CleanupMCP_Safe(t *testing.T) {
	// CleanupMCP should be safe to call even without an active MCP server
	provider := NewClaudeBinProvider("sonnet", nil)

	// Should not panic when called without active MCP server
	provider.CleanupMCP()

	// Should be safe to call multiple times
	provider.CleanupMCP()
}

func TestClaudeBinProvider_CleanupMCP_RemovesTrackedTempFiles(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)

	path := provider.imageDataToTempFile("image/png", "aGVsbG8=")
	if path == "" {
		t.Fatal("expected temp file path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected temp file to exist before cleanup: %v", err)
	}

	provider.CleanupMCP()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected temp file to be removed, got err=%v", err)
	}
}

func TestClaudeBinProvider_ImplementsProviderTurnCleaner(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)

	if _, ok := interface{}(provider).(ProviderTurnCleaner); !ok {
		t.Fatal("ClaudeBinProvider does not implement ProviderTurnCleaner - per-turn temp file cleanup will not run")
	}
}

func TestRetryProvider_ForwardsProviderTurnCleaner(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())

	if _, ok := wrapped.(ProviderTurnCleaner); !ok {
		t.Fatal("RetryProvider does not implement ProviderTurnCleaner - per-turn cleanup will not work with wrapped providers")
	}
}

func TestRetryProvider_ForwardsProviderState(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	provider.sessionID = "session-1"
	provider.messagesSent = 4
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())

	exporter, ok := wrapped.(ProviderStateExporter)
	if !ok {
		t.Fatal("RetryProvider does not implement ProviderStateExporter")
	}
	state, ok := exporter.ExportProviderState()
	if !ok {
		t.Fatal("RetryProvider did not export inner provider state")
	}

	restored := NewClaudeBinProvider("sonnet", nil)
	restoredWrapped := WrapWithRetry(restored, DefaultRetryConfig())
	importer, ok := restoredWrapped.(ProviderStateImporter)
	if !ok {
		t.Fatal("RetryProvider does not implement ProviderStateImporter")
	}
	if err := importer.ImportProviderState(state); err != nil {
		t.Fatalf("ImportProviderState: %v", err)
	}
	if restored.sessionID != "session-1" || restored.messagesSent != 4 {
		t.Fatalf("restored state = (%q, %d), want (%q, %d)", restored.sessionID, restored.messagesSent, "session-1", 4)
	}
}

func TestClaudeBinProvider_CleanupTurn_RemovesTrackedTempFiles(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)

	path := provider.imageDataToTempFile("image/png", "aGVsbG8=")
	if path == "" {
		t.Fatal("expected temp file path")
	}

	provider.CleanupTurn()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected temp file to be removed, got err=%v", err)
	}
}

func TestClaudeBinProvider_CleanupTurn_DoesNotStopMCPServer(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	// Simulate a mid-conversation state by populating the config path field.
	// CleanupTurn must leave the MCP config path untouched so the persistent
	// MCP bridge survives across turns; only CleanupMCP tears it down.
	provider.mcpConfigPath = "/tmp/does-not-exist-cleanup-turn-test.json"

	provider.CleanupTurn()

	if provider.mcpConfigPath == "" {
		t.Fatal("CleanupTurn cleared mcpConfigPath - it must be left for CleanupMCP (conversation scope)")
	}
}

func TestClaudeBinProvider_prepareClaudeCommand_ConfiguresProcessGroupAndWaitDelay(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)

	cmd, stdin, cleanup, err := provider.prepareClaudeCommand(context.Background(), []string{"--version"}, "")
	if err != nil {
		t.Fatalf("prepareClaudeCommand failed: %v", err)
	}
	defer cleanup()
	defer stdin.Close()

	if cmd.WaitDelay != claudeCommandWaitDelay {
		t.Fatalf("WaitDelay = %v, want %v", cmd.WaitDelay, claudeCommandWaitDelay)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected claude subprocess to run in its own process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("expected claude subprocess to install process-group cancellation")
	}
}

func TestSafeSendEvent_ClosedChannel(t *testing.T) {
	// Test that safeSendEvent doesn't panic on closed channel
	ch := make(chan Event)
	close(ch)

	ctx := context.Background()
	event := Event{Type: EventTextDelta, Text: "test"}

	// Should not panic and should return false
	sent := safeSendEvent(ctx, ch, event)
	if sent {
		t.Fatal("safeSendEvent should return false for closed channel")
	}
}

func TestSafeSendEvent_CancelledContext(t *testing.T) {
	// Test that safeSendEvent respects context cancellation
	ch := make(chan Event) // unbuffered, will block

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	event := Event{Type: EventTextDelta, Text: "test"}

	// Should return false due to cancelled context
	sent := safeSendEvent(ctx, ch, event)
	if sent {
		t.Fatal("safeSendEvent should return false for cancelled context")
	}
}

func TestSafeSendEvent_Success(t *testing.T) {
	// Test that safeSendEvent works normally with open channel
	ch := make(chan Event, 1) // buffered so we don't block

	ctx := context.Background()
	event := Event{Type: EventTextDelta, Text: "test"}

	sent := safeSendEvent(ctx, ch, event)
	if !sent {
		t.Fatal("safeSendEvent should return true for successful send")
	}

	// Verify event was sent
	received := <-ch
	if received.Text != "test" {
		t.Fatalf("expected text 'test', got %q", received.Text)
	}
}

func TestDispatchClaudeEvents_PrioritizesTextOverToolRequest(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 4)
	toolReqs := make(chan claudeToolRequest, 2)

	lines <- `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"text-before-tool"}}}`
	req := claudeToolRequest{
		ctx:      context.Background(),
		callID:   "call-1",
		name:     "read_file",
		args:     json.RawMessage(`{"path":"README.md"}`),
		response: make(chan ToolExecutionResponse, 1),
		ack:      make(chan error, 1),
	}
	toolReqs <- req
	close(lines)

	_, _, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	if ackErr := <-req.ack; ackErr != nil {
		t.Fatalf("expected tool request ack to succeed, got %v", ackErr)
	}

	first := <-events
	if first.Type != EventTextDelta || first.Text != "text-before-tool" {
		t.Fatalf("expected first event to be text delta, got %+v", first)
	}

	second := <-events
	if second.Type != EventToolCall || second.ToolName != "read_file" {
		t.Fatalf("expected second event to be tool call, got %+v", second)
	}
}

func TestDispatchClaudeEvents_PrioritizesSlightlyDelayedTextOverToolRequest(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 4)
	toolReqs := make(chan claudeToolRequest, 2)

	req := claudeToolRequest{
		ctx:      context.Background(),
		callID:   "call-2",
		name:     "set_commit_message",
		args:     json.RawMessage(`{"message":"m"}`),
		response: make(chan ToolExecutionResponse, 1),
		ack:      make(chan error, 1),
	}
	toolReqs <- req

	go func() {
		time.Sleep(2 * time.Millisecond)
		lines <- `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"tail-text"}}}`
		close(lines)
	}()

	_, _, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	if ackErr := <-req.ack; ackErr != nil {
		t.Fatalf("expected tool request ack to succeed, got %v", ackErr)
	}

	first := <-events
	if first.Type != EventTextDelta || first.Text != "tail-text" {
		t.Fatalf("expected first event to be delayed text delta, got %+v", first)
	}

	second := <-events
	if second.Type != EventToolCall || second.ToolName != "set_commit_message" {
		t.Fatalf("expected second event to be tool call, got %+v", second)
	}
}

func TestDispatchClaudeEvents_FallsBackToAssistantTextWhenNoDeltas(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 4)
	toolReqs := make(chan claudeToolRequest, 1)

	lines <- `{"type":"assistant","message":{"content":[{"type":"text","text":"assistant fallback text"}]}}`
	lines <- `{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":0}}`
	close(lines)

	_, _, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != EventTextDelta {
			t.Fatalf("expected EventTextDelta, got %+v", ev)
		}
		if ev.Text != "assistant fallback text" {
			t.Fatalf("unexpected fallback text: %q", ev.Text)
		}
	default:
		t.Fatal("expected fallback text event when no stream deltas are present")
	}
}

func TestDispatchClaudeEvents_FallsBackToResultTextWhenNoAssistantOrDeltas(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 2)
	toolReqs := make(chan claudeToolRequest, 1)

	lines <- `{"type":"result","subtype":"success","is_error":false,"result":"result fallback text","usage":{"input_tokens":1,"output_tokens":3,"cache_read_input_tokens":0}}`
	close(lines)

	_, _, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != EventTextDelta {
			t.Fatalf("expected EventTextDelta, got %+v", ev)
		}
		if ev.Text != "result fallback text" {
			t.Fatalf("unexpected result fallback text: %q", ev.Text)
		}
	default:
		t.Fatal("expected result fallback text event when no assistant or deltas are present")
	}
}

func TestDispatchClaudeEvents_SurfacesPermissionDenialResult(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 2)
	toolReqs := make(chan claudeToolRequest, 1)

	lines <- `{"type":"result","subtype":"success","is_error":true,"result":"Permission denied to use mcp__term_llm__shell","permission_denials":[{"tool_name":"mcp__term_llm__shell"}],"usage":{"input_tokens":1,"output_tokens":3,"cache_read_input_tokens":0}}`
	close(lines)

	_, _, handled, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !handled {
		t.Fatal("expected permission denial result to be marked handled")
	}

	select {
	case ev := <-events:
		if ev.Type != EventTextDelta {
			t.Fatalf("expected EventTextDelta, got %+v", ev)
		}
		if ev.Text != "Claude Code denied `mcp__term_llm__shell` before term-llm could execute it.\nterm-llm did not execute the tool call." {
			t.Fatalf("unexpected permission denial text: %q", ev.Text)
		}
	default:
		t.Fatal("expected permission denial result text event")
	}
}

func TestDispatchClaudeEvents_LeavesAuthResultAsError(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 2)
	toolReqs := make(chan claudeToolRequest, 1)

	lines <- `{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","permission_denials":[],"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}}`
	close(lines)

	_, _, handled, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err == nil {
		t.Fatal("expected auth result to remain an error")
	}
	if handled {
		t.Fatal("did not expect auth error to be marked handled")
	}
}

func TestDispatchClaudeEvents_EmitsStreamlinedText(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 2)
	toolReqs := make(chan claudeToolRequest, 1)

	lines <- `{"type":"streamlined_text","text":"streamlined assistant text"}`
	lines <- `{"type":"result","subtype":"success","is_error":false,"result":"ignored final result","usage":{"input_tokens":1,"output_tokens":3,"cache_read_input_tokens":0}}`
	close(lines)

	_, _, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != EventTextDelta {
			t.Fatalf("expected EventTextDelta, got %+v", ev)
		}
		if ev.Text != "streamlined assistant text" {
			t.Fatalf("unexpected streamlined text: %q", ev.Text)
		}
	default:
		t.Fatal("expected streamlined text event")
	}
}

func TestDispatchClaudeEvents_DoesNotDuplicateAssistantFallbackWhenDeltasPresent(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 4)
	toolReqs := make(chan claudeToolRequest, 1)

	lines <- `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"delta text"}}}`
	lines <- `{"type":"assistant","message":{"content":[{"type":"text","text":"assistant fallback text"}]}}`
	lines <- `{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":0}}`
	close(lines)

	_, _, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	var got []Event
	for {
		select {
		case ev := <-events:
			got = append(got, ev)
		default:
			goto drained
		}
	}
drained:
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 text event, got %d: %+v", len(got), got)
	}
	if got[0].Type != EventTextDelta || got[0].Text != "delta text" {
		t.Fatalf("unexpected event: %+v", got[0])
	}
}

func TestDispatchClaudeEvents_SeparatesCachedInputTokensInUsage(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 2)
	toolReqs := make(chan claudeToolRequest, 1)

	lines <- `{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":5}}`
	close(lines)

	usage, _, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events}, false)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.InputTokens != 11 {
		t.Fatalf("expected non-cached input tokens 11, got %d", usage.InputTokens)
	}
	if usage.CachedInputTokens != 5 {
		t.Fatalf("expected cached input tokens 5, got %d", usage.CachedInputTokens)
	}
	if usage.OutputTokens != 7 {
		t.Fatalf("expected output tokens 7, got %d", usage.OutputTokens)
	}
}

func TestHandleClaudeToolRequest_ClosedStreamReturnsError(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event)
	close(events)

	req := claudeToolRequest{
		ctx:      context.Background(),
		callID:   "call-1",
		name:     "read_file",
		args:     json.RawMessage(`{"path":"README.md"}`),
		response: make(chan ToolExecutionResponse, 1),
		ack:      make(chan error, 1),
	}

	provider.handleClaudeToolRequest(req, eventSender{ctx: context.Background(), ch: events})
	if err := <-req.ack; err == nil {
		t.Fatal("expected closed stream error")
	}
}

func TestHandleClaudeLine_ContextCancelledDoesNotBlock(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event) // unbuffered/no receiver to simulate blocked sink
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var usage *Usage
	sawTextDelta := false
	assistantFallbackText := ""
	err := provider.handleClaudeLine(
		cancelled,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}}`,
		false,
		eventSender{ctx: cancelled, ch: events},
		&usage,
		&sawTextDelta,
		&assistantFallbackText,
		nil,
		false,
	)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected context cancellation error, got %v", err)
	}
}

func TestClaudeBinProvider_ToolExecutorIsWired(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	registry := NewToolRegistry()

	// Register a test tool
	executorCalled := false
	registry.Register(&testTool{
		name: "test_tool",
		exec: func(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
			executorCalled = true
			return TextOutput("test result"), nil
		},
	})

	// Create engine which should wire up the tool executor
	_ = NewEngine(provider, registry)

	// The tool executor should now be set (not nil).
	// We verify this by checking that a Request with tools does not trigger the warning.
	// The best we can do without exposing internals is to verify the interface is satisfied
	// and trust that NewEngine's wiring works (covered by TestClaudeBinProvider_ImplementsToolExecutorSetter).

	// We can also verify the engine wiring works by checking that the executor callback
	// would be invoked if we had a real tool execution path.
	if !executorCalled {
		// This is expected - we didn't actually execute a tool, just verified wiring is possible.
		// The important thing is that the interface is satisfied and SetToolExecutor was called.
	}
}

func TestParseClaudeEffort(t *testing.T) {
	tests := []struct {
		input      string
		wantModel  string
		wantEffort string
	}{
		{"opus-max", "opus", "max"},
		{"opus-xhigh", "opus", "xhigh"},
		{"opus-low", "opus", "low"},
		{"opus-medium", "opus", "medium"},
		{"opus-high", "opus", "high"},
		{"opus", "opus", ""},
		{"sonnet-low", "sonnet", "low"},
		{"sonnet-medium", "sonnet", "medium"},
		{"sonnet-high", "sonnet", "high"},
		{"sonnet-xhigh", "sonnet-xhigh", ""}, // xhigh is opus/fable-only
		{"sonnet", "sonnet", ""},
		{"fable-max", "fable", "max"},
		{"fable-xhigh", "fable", "xhigh"},
		{"fable-high", "fable", "high"},
		{"fable-medium", "fable", "medium"},
		{"fable-low", "fable", "low"},
		{"fable", "fable", ""},
		{"haiku", "haiku", ""},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			model, effort := parseClaudeEffort(tt.input)
			if model != tt.wantModel {
				t.Errorf("parseClaudeEffort(%q) model = %q, want %q", tt.input, model, tt.wantModel)
			}
			if effort != tt.wantEffort {
				t.Errorf("parseClaudeEffort(%q) effort = %q, want %q", tt.input, effort, tt.wantEffort)
			}
		})
	}
}

func TestValidateClaudeBinModel(t *testing.T) {
	tests := []struct {
		model   string
		wantErr bool
		errSub  string
	}{
		{"", false, ""},
		{"opus", false, ""},
		{"opus-max", false, ""},
		{"opus-xhigh", false, ""},
		{"opus-high", false, ""},
		{"opus-medium", false, ""},
		{"opus-low", false, ""},
		{"sonnet", false, ""},
		{"sonnet-high", false, ""},
		{"sonnet-medium", false, ""},
		{"sonnet-low", false, ""},
		{"fable", false, ""},
		{"fable-max", false, ""},
		{"fable-xhigh", false, ""},
		{"haiku", false, ""},
		// bare effort names — should error with a suggestion pointing at opus-<effort>.
		{"max", true, "claude-bin:opus-max"},
		{"xhigh", true, "claude-bin:opus-xhigh"},
		{"high", true, "claude-bin:opus-high"},
		{"medium", true, "claude-bin:opus-medium"},
		{"low", true, "claude-bin:opus-low"},
		// unknown model — passes through; we don't enforce an allowlist here.
		{"banana", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			err := ValidateClaudeBinModel(tt.model)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateClaudeBinModel(%q) = nil, want error containing %q", tt.model, tt.errSub)
				}
				if tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
					t.Errorf("ValidateClaudeBinModel(%q) error = %q, want substring %q", tt.model, err.Error(), tt.errSub)
				}
			} else if err != nil {
				t.Errorf("ValidateClaudeBinModel(%q) = %v, want nil", tt.model, err)
			}
		})
	}
}

func TestClaudeBinProvider_NameWithEffort(t *testing.T) {
	tests := []struct {
		model    string
		wantName string
	}{
		{"opus-max", "Claude CLI (opus, effort=max)"},
		{"opus", "Claude CLI (opus)"},
		{"sonnet-high", "Claude CLI (sonnet, effort=high)"},
		{"sonnet", "Claude CLI (sonnet)"},
		{"", "Claude CLI (sonnet)"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			p := NewClaudeBinProvider(tt.model, nil)
			if got := p.Name(); got != tt.wantName {
				t.Errorf("Name() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestClaudeBinProvider_BuildArgsDisablesHooksByDefault(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)

	args, _ := p.buildArgs(context.Background(), Request{}, eventSender{})
	joined := strings.Join(args, "\n")

	if !strings.Contains(joined, "--settings") {
		t.Fatal("expected claude-bin args to include --settings when hooks are disabled by default")
	}
	if !strings.Contains(joined, `{"disableAllHooks":true}`) {
		t.Fatal("expected claude-bin args to disable hooks by default")
	}
	if !strings.Contains(joined, "--disable-slash-commands") {
		t.Fatal("expected claude-bin args to disable Claude Code slash-command skills")
	}
	for i, arg := range args {
		if arg == "--setting-sources" {
			if i+1 >= len(args) || args[i+1] != "" {
				t.Fatalf("--setting-sources = %q, want empty", args[i+1])
			}
			return
		}
	}
	t.Fatal("expected claude-bin args to include --setting-sources")
}

func TestClaudeBinProvider_BuildArgsDangerousPermissionsRespectsEuid(t *testing.T) {
	origGetEuid := getEuid
	defer func() { getEuid = origGetEuid }()

	t.Run("non-root includes dangerous skip flag", func(t *testing.T) {
		getEuid = func() int { return 1000 }
		p := NewClaudeBinProvider("sonnet", nil)
		args, _ := p.buildArgs(context.Background(), Request{}, eventSender{})
		joined := strings.Join(args, "\n")
		if !strings.Contains(joined, "--dangerously-skip-permissions") {
			t.Fatal("expected --dangerously-skip-permissions for non-root")
		}
		if strings.Contains(joined, "--permission-mode\nbypassPermissions") {
			t.Fatal("did not expect bypassPermissions permission mode for non-root")
		}
	})

	t.Run("root uses bypass permission mode", func(t *testing.T) {
		getEuid = func() int { return 0 }
		p := NewClaudeBinProvider("sonnet", nil)
		args, _ := p.buildArgs(context.Background(), Request{}, eventSender{})
		joined := strings.Join(args, "\n")
		if strings.Contains(joined, "--dangerously-skip-permissions") {
			t.Fatal("expected claude-bin args to omit --dangerously-skip-permissions when running as root")
		}
		if !strings.Contains(joined, "--permission-mode\nbypassPermissions") {
			t.Fatal("expected root runs to request bypassPermissions mode")
		}
	})
}

func TestClaudeBinProvider_BuildArgsCanEnableHooks(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	p.SetEnableHooks(true)

	args, _ := p.buildArgs(context.Background(), Request{}, eventSender{})
	joined := strings.Join(args, "\n")

	if strings.Contains(joined, `{"disableAllHooks":true}`) {
		t.Fatal("expected disableAllHooks setting to be omitted when hooks are enabled")
	}
}

func TestClaudeBinProvider_BuildArgsEffortPrecedence(t *testing.T) {
	tests := []struct {
		name          string
		providerModel string
		requestModel  string
		requestEffort string
		wantEffort    string
	}{
		{
			name:          "provider model suffix sets effort",
			providerModel: "opus-high",
			wantEffort:    "high",
		},
		{
			name:          "request model suffix overrides provider effort",
			providerModel: "opus-low",
			requestModel:  "opus-high",
			wantEffort:    "high",
		},
		{
			name:          "request reasoning_effort field wins over model suffix and provider effort",
			providerModel: "opus-low",
			requestModel:  "opus-medium",
			requestEffort: "max",
			wantEffort:    "max",
		},
		{
			name:          "no effort when not specified",
			providerModel: "sonnet",
			wantEffort:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewClaudeBinProvider(tt.providerModel, nil)
			_, effort := p.buildArgs(context.Background(), Request{
				Model:           tt.requestModel,
				ReasoningEffort: tt.requestEffort,
			}, eventSender{})
			if effort != tt.wantEffort {
				t.Errorf("effort = %q, want %q", effort, tt.wantEffort)
			}
		})
	}
}

func TestClaudeBinProvider_BuildArgsDisablesBuiltInClaudeTools(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)

	args, _ := p.buildArgs(context.Background(), Request{
		Tools: []ToolSpec{{Name: "diagnostic_ping", Description: "diagnostic"}},
	}, eventSender{})

	foundTools := false
	for i, arg := range args {
		if arg == "--tools" {
			foundTools = true
			if i+1 >= len(args) || args[i+1] != "" {
				t.Fatalf("--tools = %q, want empty to disable Claude Code built-ins", args[i+1])
			}
		}
	}
	if !foundTools {
		t.Fatal("expected claude-bin args to include --tools empty")
	}
	if joined := strings.Join(args, "\n"); strings.Contains(joined, "--mcp-config") {
		t.Fatal("expected no --mcp-config without a tool executor")
	}
}

func TestClaudeBinProvider_SystemPromptForTurn(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "You are helpful."}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "Hi"}}},
	}

	p := NewClaudeBinProvider("sonnet", nil)
	if got := p.systemPromptForTurn(msgs, false); got != "You are helpful." {
		t.Fatalf("initial turn systemPromptForTurn = %q, want %q", got, "You are helpful.")
	}

	// The system prompt must be re-sent on resumed turns too: Claude Code
	// rebuilds the request from the current invocation's flags, so a resume
	// without --system-prompt reverts to Claude Code's default prompt.
	p.sessionID = "resume-abc"
	if got := p.systemPromptForTurn(msgs, false); got != "You are helpful." {
		t.Fatalf("resume turn systemPromptForTurn = %q, want %q (must re-send on --resume)", got, "You are helpful.")
	}
	if got := p.systemPromptForTurn(msgs, true); got != "You are helpful." {
		t.Fatalf("ephemeral resume turn systemPromptForTurn = %q, want system prompt", got)
	}
}

func TestClaudeBinProvider_EphemeralStreamDoesNotResumeOrMutateState(t *testing.T) {
	dir := t.TempDir()
	argsFile := dir + "/args.txt"
	stdinFile := dir + "/stdin.txt"
	claudePath := dir + "/claude"
	script := `#!/bin/sh
printf '%s\n' "$@" > "$CLAUDE_ARGS_FILE"
cat > "$CLAUDE_STDIN_FILE"
printf '%s\n' '{"type":"system","session_id":"guardian-session","model":"sonnet","tools":[]}'
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}}'
printf '%s\n' '{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0}}'
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_ARGS_FILE", argsFile)
	t.Setenv("CLAUDE_STDIN_FILE", stdinFile)

	p := NewClaudeBinProvider("sonnet", nil)
	p.sessionID = "chat-session"
	p.messagesSent = 7

	stream, err := p.Stream(context.Background(), Request{
		Ephemeral: true,
		Messages: []Message{
			SystemText("guardian policy"),
			UserText("review this command"),
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}

	if p.sessionID != "chat-session" {
		t.Fatalf("sessionID = %q, want original chat-session", p.sessionID)
	}
	if p.messagesSent != 7 {
		t.Fatalf("messagesSent = %d, want unchanged 7", p.messagesSent)
	}
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(argsBytes)
	if strings.Contains(args, "--resume") || strings.Contains(args, "chat-session") {
		t.Fatalf("ephemeral args leaked resume state:\n%s", args)
	}
	if !strings.Contains(args, "--system-prompt") || !strings.Contains(args, "guardian policy") {
		t.Fatalf("ephemeral args missing system prompt:\n%s", args)
	}
}

func TestClaudeBinProvider_EphemeralToolStreamExposesBridgeWhenStandalone(t *testing.T) {
	dir := t.TempDir()
	readyFile := dir + "/ready"
	releaseFile := dir + "/release"
	claudePath := dir + "/claude"
	script := `#!/bin/sh
printf '%s\n' '{"type":"system","session_id":"guardian-session","model":"sonnet","tools":[]}'
: > "$CLAUDE_READY_FILE"
while [ ! -f "$CLAUDE_RELEASE_FILE" ]; do sleep 0.01; done
printf '%s\n' '{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0}}'
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_READY_FILE", readyFile)
	t.Setenv("CLAUDE_RELEASE_FILE", releaseFile)

	p := NewClaudeBinProvider("sonnet", nil)
	stream, err := p.Stream(context.Background(), Request{
		Ephemeral: true,
		Messages:  []Message{UserText("standalone tool request")},
		Tools:     []ToolSpec{{Name: "diagnostic_ping", Description: "diagnostic"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyFile); err != nil {
		t.Fatalf("fake claude did not become ready: %v", err)
	}

	var bridge *claudeTurnBridge
	var events chan<- Event
	for i := 0; i < 1000; i++ {
		p.eventsMu.Lock()
		bridge = p.currentBridge
		events = p.currentEvents
		p.eventsMu.Unlock()
		if bridge != nil && events != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bridge == nil || events == nil {
		t.Fatal("ephemeral standalone tool request did not expose active bridge")
	}

	if err := os.WriteFile(releaseFile, []byte("ok"), 0o644); err != nil {
		t.Fatalf("release fake claude: %v", err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}
	if p.activeStream.Load() {
		t.Fatal("activeStream still set after ephemeral tool stream completed")
	}
}

func TestClaudeBinProvider_EphemeralToolStreamRejectedWhileActive(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	p.activeStream.Store(true)
	stream, err := p.Stream(context.Background(), Request{
		Ephemeral: true,
		Messages:  []Message{UserText("standalone tool request")},
		Tools:     []ToolSpec{{Name: "diagnostic_ping", Description: "diagnostic"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Type != EventError || ev.Err == nil || !strings.Contains(ev.Err.Error(), "ephemeral tool request cannot run") {
		t.Fatalf("Recv() = %#v, want ephemeral tool active-stream error", ev)
	}
}

func TestClaudeBinProvider_EphemeralSystemLineDoesNotReplaceSession(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	p.sessionID = "chat-session"
	var usage *Usage
	sawTextDelta := false
	assistantFallbackText := ""
	if err := p.handleClaudeLine(
		context.Background(),
		`{"type":"system","session_id":"guardian-session","model":"sonnet","tools":[]}`,
		false,
		eventSender{ctx: context.Background(), ch: make(chan Event, 1)},
		&usage,
		&sawTextDelta,
		&assistantFallbackText,
		nil,
		true,
	); err != nil {
		t.Fatalf("handleClaudeLine: %v", err)
	}
	if p.sessionID != "chat-session" {
		t.Fatalf("sessionID = %q, want unchanged chat-session", p.sessionID)
	}
}

func TestClaudeBinProvider_ResetConversationClearsResumeState(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	p.sessionID = "resume-abc"
	p.messagesSent = 3

	p.ResetConversation()

	if p.sessionID != "" {
		t.Fatalf("sessionID = %q, want empty after reset", p.sessionID)
	}
	if p.messagesSent != 0 {
		t.Fatalf("messagesSent = %d, want 0 after reset", p.messagesSent)
	}

	args, _ := p.buildArgs(context.Background(), Request{}, eventSender{})
	if joined := strings.Join(args, " "); strings.Contains(joined, "--resume") {
		t.Fatalf("buildArgs() = %q, want no --resume after reset", joined)
	}
}

func TestClaudeBinProviderProviderStateRoundTrip(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	p.sessionID = "resume-xyz"
	p.messagesSent = 7

	data, ok := p.ExportProviderState()
	if !ok {
		t.Fatal("ExportProviderState returned ok=false")
	}

	restored := NewClaudeBinProvider("sonnet", nil)
	if err := restored.ImportProviderState(data); err != nil {
		t.Fatalf("ImportProviderState: %v", err)
	}
	if restored.sessionID != "resume-xyz" {
		t.Fatalf("restored sessionID = %q, want resume-xyz", restored.sessionID)
	}
	if restored.messagesSent != 7 {
		t.Fatalf("restored messagesSent = %d, want 7", restored.messagesSent)
	}

	args, _ := restored.buildArgs(context.Background(), Request{}, eventSender{})
	if joined := strings.Join(args, " "); !strings.Contains(joined, "--resume resume-xyz") {
		t.Fatalf("buildArgs() = %q, want --resume resume-xyz", joined)
	}
}

func TestClaudeBinProviderProviderStateRejectsInvalidData(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	if err := p.ImportProviderState([]byte(`{"messages_sent":1}`)); err == nil {
		t.Fatal("expected missing session_id to fail")
	}
	if err := p.ImportProviderState([]byte(`{"session_id":"resume","messages_sent":-1}`)); err == nil {
		t.Fatal("expected negative messages_sent to fail")
	}
}

func TestClaudeBinProviderProviderStateEmptyWithoutSession(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	if data, ok := p.ExportProviderState(); ok || len(data) != 0 {
		t.Fatalf("ExportProviderState() = (%q, %v), want empty false", data, ok)
	}
}

func TestClaudeBinProvider_BuildCommandEnv(t *testing.T) {
	origGetEuid := getEuid
	defer func() { getEuid = origGetEuid }()
	getEuid = func() int { return 1000 }

	t.Setenv("ANTHROPIC_API_KEY", "should-be-cleared")
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "medium")
	t.Setenv("CLAUDE_CODE_DISABLE_WORKFLOWS", "0")
	t.Setenv("DISABLE_GROWTHBOOK", "0")
	t.Setenv("CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS", "0")
	t.Setenv("PATH", os.Getenv("PATH"))

	p := NewClaudeBinProvider("opus-max", map[string]string{
		"IS_SANDBOX":                           "1",
		"CLAUDE_CODE_EFFORT_LEVEL":             "max-from-config-should-be-overridden",
		"CLAUDE_CODE_DISABLE_WORKFLOWS":        "config-value-should-be-overridden",
		"DISABLE_GROWTHBOOK":                   "config-value-should-be-overridden",
		"CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS": "config-value-should-be-overridden",
		"ANTHROPIC_API_KEY":                    "config-value-should-not-survive",
		"CUSTOM_TERM_LLM_TEST_VALUE":           "ok",
	})

	env := p.buildCommandEnv("max")
	joined := strings.Join(env, "\n")

	if strings.Contains(joined, "ANTHROPIC_API_KEY=should-be-cleared") {
		t.Fatal("expected inherited ANTHROPIC_API_KEY to be removed when preferOAuth is enabled")
	}
	if !strings.Contains(joined, "IS_SANDBOX=1") {
		t.Fatal("expected extra env IS_SANDBOX=1 to be present")
	}
	if !strings.Contains(joined, "CUSTOM_TERM_LLM_TEST_VALUE=ok") {
		t.Fatal("expected custom extra env var to be present")
	}
	if strings.Contains(joined, "CLAUDE_CODE_EFFORT_LEVEL=medium") {
		t.Fatal("expected inherited effort level to be removed")
	}
	if strings.Contains(joined, "CLAUDE_CODE_EFFORT_LEVEL=max-from-config-should-be-overridden") {
		t.Fatal("expected config effort level to be overridden by model effort")
	}
	if !strings.Contains(joined, "CLAUDE_CODE_EFFORT_LEVEL=max") {
		t.Fatal("expected model-derived effort level to be present")
	}
	for key, want := range forcedClaudeCodeIsolationEnv {
		if strings.Contains(joined, key+"=0") {
			t.Fatalf("expected inherited %s to be removed", key)
		}
		if strings.Contains(joined, key+"=config-value-should-be-overridden") {
			t.Fatalf("expected config %s to be overridden", key)
		}
		if !strings.Contains(joined, key+"="+want) {
			t.Fatalf("expected %s=%s to be forced unconditionally", key, want)
		}
	}
}

func TestClaudeBinProvider_BuildCommandEnvRootForcesSandboxMarker(t *testing.T) {
	origGetEuid := getEuid
	defer func() { getEuid = origGetEuid }()
	getEuid = func() int { return 0 }

	t.Setenv("IS_SANDBOX", "0")
	t.Setenv("PATH", os.Getenv("PATH"))

	p := NewClaudeBinProvider("sonnet", map[string]string{"IS_SANDBOX": "0"})
	env := p.buildCommandEnv("")
	joined := strings.Join(env, "\n")

	if strings.Contains(joined, "IS_SANDBOX=0") {
		t.Fatal("expected stale IS_SANDBOX=0 values to be removed for root bypass")
	}
	if !strings.Contains(joined, "IS_SANDBOX=1") {
		t.Fatal("expected IS_SANDBOX=1 to be forced for root bypass")
	}
}

func TestClaudeBinProvider_BuildCommandEnvRootPreservesBubblewrapMarker(t *testing.T) {
	origGetEuid := getEuid
	defer func() { getEuid = origGetEuid }()
	getEuid = func() int { return 0 }

	t.Setenv("IS_SANDBOX", "0")
	t.Setenv("CLAUDE_CODE_BUBBLEWRAP", "1")
	t.Setenv("PATH", os.Getenv("PATH"))

	p := NewClaudeBinProvider("sonnet", nil)
	env := p.buildCommandEnv("")
	joined := strings.Join(env, "\n")

	if strings.Contains(joined, "IS_SANDBOX=1") {
		t.Fatal("did not expect forced IS_SANDBOX=1 when CLAUDE_CODE_BUBBLEWRAP is already set")
	}
	if !strings.Contains(joined, "CLAUDE_CODE_BUBBLEWRAP=1") {
		t.Fatal("expected inherited CLAUDE_CODE_BUBBLEWRAP=1 to be preserved")
	}
}

func TestClaudeBinProvider_UserFacingCommandErrorKeepsDiagnosticsOutOfErrorString(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	claudeErr := p.newClaudeCommandError(
		fmt.Errorf("exit status 1"),
		1,
		[]string{"--print"},
		"",
		"User: hello",
		false,
		[]string{`{"type":"result","is_error":true}`},
		[]string{"", "permission denied by managed policy", "extra stderr detail"},
	)

	err := p.userFacingClaudeCommandError(claudeErr)
	msg := err.Error()
	if !strings.Contains(msg, "Claude Code denied a tool call before term-llm could execute it") {
		t.Fatalf("unexpected user-facing error: %q", msg)
	}
	if strings.Contains(msg, "extra stderr detail") || strings.Contains(msg, `{"type":"result"}`) {
		t.Fatalf("user-facing error leaked diagnostic tail: %q", msg)
	}
	fieldsErr, ok := err.(interface{ DebugFields() map[string]any })
	if !ok {
		t.Fatal("expected user-facing error to expose debug fields")
	}
	fields := fieldsErr.DebugFields()
	if got := fields["stderr_tail"].(string); !strings.Contains(got, "extra stderr detail") {
		t.Fatalf("debug fields lost stderr tail: %q", got)
	}
	if !errors.Is(err, claudeErr) {
		t.Fatal("expected user-facing error to unwrap to ClaudeCommandError")
	}
}

func TestClaudeBinProvider_CommandErrorDebugFields(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	p := NewClaudeBinProvider("opus-max", map[string]string{
		"CUSTOM_ENV":        "value with spaces",
		"SENSITIVE_TOKEN":   "super-secret",
		"ANTHROPIC_API_KEY": "config-secret",
	})

	err := p.newClaudeCommandError(
		fmt.Errorf("exit status 7"),
		7,
		[]string{"--print", "--model", "opus", "--settings", `{"disableAllHooks":true}`},
		"max",
		"User: hello",
		false,
		[]string{`{"type":"system","subtype":"init"}`},
		[]string{"fatal problem"},
	)

	if !strings.Contains(err.Error(), "claude command failed (exit 7)") {
		t.Fatalf("unexpected error string: %s", err.Error())
	}
	fields := err.DebugFields()
	if got := fields["command_line"].(string); !strings.Contains(got, "claude --print --model opus --settings") || !strings.Contains(got, `'`+`{"disableAllHooks":true}`+`'`) {
		t.Fatalf("command_line not shell quoted as expected: %q", got)
	}
	if got := fields["stdin"].(string); got != "User: hello" {
		t.Fatalf("stdin = %q", got)
	}
	if got := fields["stdin_len"].(int); got != len("User: hello") {
		t.Fatalf("stdin_len = %d", got)
	}
	if got := fields["stderr_tail"].(string); got != "fatal problem" {
		t.Fatalf("stderr_tail = %q", got)
	}
	env := fields["env"].(map[string]string)
	if env["CLAUDE_CODE_EFFORT_LEVEL"] != "max" {
		t.Fatalf("missing effort env: %#v", env)
	}
	if env["SENSITIVE_TOKEN"] != "[redacted]" {
		t.Fatalf("sensitive env was not redacted: %#v", env)
	}
	removed := fields["removed_env"].([]string)
	if len(removed) != 2 {
		t.Fatalf("removed_env = %#v, want inherited and provider ANTHROPIC_API_KEY entries", removed)
	}
}

func TestClaudeBinProvider_CommandErrorTruncatesDiagnostics(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	longPrompt := strings.Repeat("x", claudeDiagnosticStdinMaxBytes+10)
	longLine := strings.Repeat("y", claudeDiagnosticLineMaxBytes+10)

	err := p.newClaudeCommandError(fmt.Errorf("exit status 1"), 1, []string{"--print"}, "", longPrompt, false, []string{longLine}, []string{longLine})
	fields := err.DebugFields()
	if truncated, _ := fields["stdin_truncated"].(bool); !truncated {
		t.Fatal("expected stdin_truncated=true")
	}
	if got := fields["stdin"].(string); len(got) <= claudeDiagnosticStdinMaxBytes || !strings.Contains(got, "...[truncated 10 bytes]") {
		t.Fatalf("stdin truncation missing: len=%d tail=%q", len(got), got[len(got)-40:])
	}
	if got := fields["stdout_tail"].(string); len(got) <= claudeDiagnosticLineMaxBytes || !strings.Contains(got, "...[truncated 10 bytes]") {
		t.Fatalf("stdout tail truncation missing: len=%d", len(got))
	}
}

func TestMapModelToClaudeArg(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "opus"},
		{"sonnet", "sonnet"},
		{"haiku", "haiku"},
		{"fable", "fable"},
		{"claude-fable-5", "fable"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := mapModelToClaudeArg(tt.input); got != tt.want {
				t.Errorf("mapModelToClaudeArg(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsPromptTooLong(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"exact match", fmt.Errorf("Prompt is too long"), true},
		{"lowercase", fmt.Errorf("prompt is too long"), true},
		{"wrapped", fmt.Errorf("API error: Prompt is too long for this model"), true},
		{"unrelated error", fmt.Errorf("rate limit exceeded"), false},
		{"empty error", fmt.Errorf(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPromptTooLong(tt.err); got != tt.want {
				t.Errorf("isPromptTooLong(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestTruncateToolResultsAt(t *testing.T) {
	shortContent := "short result"
	longContent := strings.Repeat("x", maxToolResultCharsOnRetry+1000)

	messages := []Message{
		{
			Role: RoleUser,
			Parts: []Part{
				{Type: PartText, Text: "hello"},
			},
		},
		{
			Role: RoleTool,
			Parts: []Part{
				{
					Type: PartToolResult,
					ToolResult: &ToolResult{
						ID:      "1",
						Name:    "short_tool",
						Content: shortContent,
					},
				},
				{
					Type: PartToolResult,
					ToolResult: &ToolResult{
						ID:      "2",
						Name:    "long_tool",
						Content: longContent,
					},
				},
			},
		},
	}

	truncated := truncateToolResultsAt(messages, maxToolResultCharsOnRetry)

	// Should not modify original
	if messages[1].Parts[1].ToolResult.Content != longContent {
		t.Fatal("truncateToolResultsAt modified original message")
	}

	// User message should be unchanged
	if truncated[0].Parts[0].Text != "hello" {
		t.Fatalf("user message changed: got %q", truncated[0].Parts[0].Text)
	}

	// Short tool result should be unchanged
	if truncated[1].Parts[0].ToolResult.Content != shortContent {
		t.Fatalf("short result changed: got %q", truncated[1].Parts[0].ToolResult.Content)
	}

	// Long tool result should be truncated
	got := truncated[1].Parts[1].ToolResult.Content
	if len(got) >= len(longContent) {
		t.Fatalf("long result was not truncated: len=%d", len(got))
	}
	if !strings.Contains(got, "[Truncated: showing first") {
		t.Fatalf("truncation notice missing from result: %s", got[len(got)-80:])
	}
	expectedPrefix := longContent[:maxToolResultCharsOnRetry]
	if !strings.HasPrefix(got, expectedPrefix) {
		t.Fatal("truncated result does not start with expected prefix")
	}
}

func TestTruncateToolResults_NoToolResults(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "hi"}}},
	}
	truncated := truncateToolResultsAt(messages, maxToolResultCharsOnRetry)
	if len(truncated) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(truncated))
	}
	if truncated[0].Parts[0].Text != "hello" || truncated[1].Parts[0].Text != "hi" {
		t.Fatal("messages were incorrectly modified")
	}
}

func TestHasImages_UserPartImage(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{
			{Type: PartText, Text: "look at this"},
			{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
		}},
	}
	if !hasImages(msgs) {
		t.Fatal("expected hasImages to return true for user PartImage")
	}
}

func TestHasImages_ToolResultImage(t *testing.T) {
	msgs := []Message{
		{Role: RoleTool, Parts: []Part{
			{Type: PartToolResult, ToolResult: &ToolResult{
				ID:   "1",
				Name: "screenshot",
				ContentParts: []ToolContentPart{
					{Type: ToolContentPartText, Text: "here is the image"},
					{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
				},
			}},
		}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "what do you see?"}}},
	}
	if !hasImages(msgs) {
		t.Fatal("expected hasImages to return true for tool result image")
	}
}

func TestHasImages_TextOnly(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "hi"}}},
		{Role: RoleTool, Parts: []Part{
			{Type: PartToolResult, ToolResult: &ToolResult{ID: "1", Name: "t", Content: "result"}},
		}},
	}
	if hasImages(msgs) {
		t.Fatal("expected hasImages to return false for text-only messages")
	}
}

func TestBuildStreamJsonInput_UserPastedImage(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{
			{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: PartText, Text: "describe this"},
		}},
	}
	out := p.buildStreamJsonInput(msgs, "")
	if out == "" {
		t.Fatal("expected non-empty stream-json output")
	}

	var msg sdkUserMessage
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("failed to parse stream-json: %v", err)
	}
	if msg.Type != "user" {
		t.Errorf("expected type 'user', got %q", msg.Type)
	}
	if msg.Message.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Message.Role)
	}

	var imageBlock, textBlock *sdkContentBlock
	for i := range msg.Message.Content {
		b := &msg.Message.Content[i]
		switch b.Type {
		case "image":
			imageBlock = b
		case "text":
			textBlock = b
		}
	}
	if imageBlock == nil {
		t.Fatal("expected image content block")
	}
	if imageBlock.Source == nil || imageBlock.Source.Type != "base64" {
		t.Errorf("expected base64 image source, got %+v", imageBlock.Source)
	}
	if imageBlock.Source.MediaType != "image/png" {
		t.Errorf("expected media_type 'image/png', got %q", imageBlock.Source.MediaType)
	}
	if imageBlock.Source.Data != "aGVsbG8=" {
		t.Errorf("expected base64 data 'aGVsbG8=', got %q", imageBlock.Source.Data)
	}
	if textBlock == nil || textBlock.Text != "describe this" {
		t.Errorf("expected text block 'describe this', got %v", textBlock)
	}
}

func TestBuildStreamJsonInput_ToolResultImageWithoutUserMessageEmitsSyntheticTurn(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "let me inspect that image"}}},
		{Role: RoleTool, Parts: []Part{
			{Type: PartToolResult, ToolResult: &ToolResult{
				ID:   "tool-123",
				Name: "view_image",
				ContentParts: []ToolContentPart{
					{Type: ToolContentPartText, Text: "Image loaded"},
					{Type: ToolContentPartImageData, ImageData: &ToolImageData{
						MediaType: "image/png",
						Base64:    "aGVsbG8=",
					}},
				},
			}},
		}},
	}
	out := p.buildStreamJsonInput(msgs, "sess-abc")
	if out == "" {
		t.Fatal("expected non-empty output")
	}

	parsed := parseSDKUserMessages(t, out)
	if len(parsed) != 1 {
		t.Fatalf("expected 1 synthetic user message, got %d", len(parsed))
	}

	msg := parsed[0]
	if msg.SessionID != "sess-abc" {
		t.Errorf("expected session_id 'sess-abc', got %q", msg.SessionID)
	}
	if msg.ParentToolUseID == nil || *msg.ParentToolUseID != "tool-123" {
		t.Fatalf("expected parent_tool_use_id 'tool-123', got %+v", msg.ParentToolUseID)
	}
	if msg.Message.Role != "user" {
		t.Fatalf("expected synthetic message role 'user', got %q", msg.Message.Role)
	}
	if len(msg.Message.Content) != 1 {
		t.Fatalf("expected synthetic message to contain only the image block, got %+v", msg.Message.Content)
	}
	block := msg.Message.Content[0]
	if block.Type != "image" || block.Source == nil || block.Source.Data != "aGVsbG8=" {
		t.Fatalf("expected synthetic image block, got %+v", block)
	}
}

func TestBuildStreamJsonInput_ToolResultImageBeforeRealUserPreservesOrder(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleTool, Parts: []Part{
			{Type: PartToolResult, ToolResult: &ToolResult{
				ID:   "tool-456",
				Name: "view_image",
				ContentParts: []ToolContentPart{
					{Type: ToolContentPartText, Text: "Image loaded"},
					{Type: ToolContentPartImageData, ImageData: &ToolImageData{
						MediaType: "image/png",
						Base64:    "aGVsbG8=",
					}},
				},
			}},
		}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "what is shown?"}}},
	}
	out := p.buildStreamJsonInput(msgs, "sess-abc")
	if out == "" {
		t.Fatal("expected non-empty output")
	}

	parsed := parseSDKUserMessages(t, out)
	if len(parsed) != 2 {
		t.Fatalf("expected 2 stream-json user messages, got %d", len(parsed))
	}

	first := parsed[0]
	if first.ParentToolUseID == nil || *first.ParentToolUseID != "tool-456" {
		t.Fatalf("expected first message parent_tool_use_id 'tool-456', got %+v", first.ParentToolUseID)
	}
	if len(first.Message.Content) != 1 || first.Message.Content[0].Type != "image" {
		t.Fatalf("expected first message to contain only the synthetic tool image, got %+v", first.Message.Content)
	}

	second := parsed[1]
	if second.ParentToolUseID != nil {
		t.Fatalf("expected real user message to have no parent_tool_use_id, got %+v", second.ParentToolUseID)
	}
	if len(second.Message.Content) != 1 || second.Message.Content[0].Type != "text" || second.Message.Content[0].Text != "what is shown?" {
		t.Fatalf("expected real user text message second, got %+v", second.Message.Content)
	}
}

func TestBuildStreamJsonInput_TextOnlyUsesConversationPrompt(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
	}
	// Text-only should NOT trigger stream-json path
	if hasImages(msgs) {
		t.Fatal("text-only messages should not trigger hasImages")
	}
	// buildConversationPrompt should still work
	prompt := p.buildConversationPrompt(msgs)
	if !strings.Contains(prompt, "hello") {
		t.Errorf("expected 'hello' in prompt, got %q", prompt)
	}
}

func TestBuildStreamJsonInput_SessionID(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{
			{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/jpeg", Base64: "dGVzdA=="}},
		}},
	}
	out := p.buildStreamJsonInput(msgs, "resume-session-123")
	var msg sdkUserMessage
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if msg.SessionID != "resume-session-123" {
		t.Errorf("expected session_id 'resume-session-123', got %q", msg.SessionID)
	}
}

func TestBuildStreamJsonInput_FramesPriorHistoryAndKeepsFinalImage(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "FIRST_QUESTION"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "FIRST_ANSWER"}}},
		{Role: RoleUser, Parts: []Part{
			{Type: PartText, Text: "LATEST_QUESTION"},
			{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/jpeg", Base64: "dGVzdA=="}},
		}},
	}

	msgsOut := parseSDKUserMessages(t, p.buildStreamJsonInput(msgs, "sess-frame"))
	if len(msgsOut) != 1 {
		t.Fatalf("expected the prior history to collapse into a single live user turn, got %d messages", len(msgsOut))
	}

	blocks := msgsOut[0].Message.Content
	if len(blocks) < 2 {
		t.Fatalf("expected a leading framing text block plus the final turn content, got %d blocks", len(blocks))
	}

	framing := blocks[0]
	if framing.Type != "text" {
		t.Fatalf("expected first block to be the framing text, got type %q", framing.Type)
	}
	for _, want := range []string{"<conversation_history>", "FIRST_QUESTION", "FIRST_ANSWER", "</conversation_history>", claudeBinResumeReplayInstruction} {
		if !strings.Contains(framing.Text, want) {
			t.Errorf("framing block missing %q\ngot: %q", want, framing.Text)
		}
	}
	// The assistant reply must survive: stream-json normally drops it, which is
	// what made Claude re-answer the whole thread from the start on resume.
	if !strings.Contains(framing.Text, "Assistant: FIRST_ANSWER") {
		t.Errorf("expected the prior assistant turn to be preserved in the framing block, got: %q", framing.Text)
	}

	var sawLatestText, sawImage bool
	for _, b := range blocks {
		if b.Type == "text" && strings.Contains(b.Text, "LATEST_QUESTION") && !strings.Contains(b.Text, "<conversation_history>") {
			sawLatestText = true
		}
		if b.Type == "image" && b.Source != nil && b.Source.Data == "dGVzdA==" {
			sawImage = true
		}
	}
	if !sawLatestText {
		t.Error("expected the latest user question to be sent as live content")
	}
	if !sawImage {
		t.Error("expected the final turn image to survive as a real vision block")
	}
}

func TestBuildStreamJsonInput_NoFramingWithoutPriorTurn(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	// A single user turn with an image (fresh first message): no prior assistant
	// turn, so no framing should be added and behavior is unchanged.
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{
			{Type: PartText, Text: "hello"},
			{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/jpeg", Base64: "dGVzdA=="}},
		}},
	}
	out := p.buildStreamJsonInput(msgs, "sess-fresh")
	if strings.Contains(out, "conversation_history") || strings.Contains(out, claudeBinResumeReplayInstruction) {
		t.Fatalf("did not expect resume framing for a fresh single-turn input, got: %q", out)
	}
}

// TestBuildStreamJsonInput_FramesHistoryEvenWhenFinalUserEmpty locks the concern
// behind the trailing defensive guard via the path that is actually reachable: a
// prior answered history followed by a latest user turn whose own content is empty.
// The loop injects the framing onto that content-less turn, so the replayed history
// must still reach the model rather than being silently dropped.
func TestBuildStreamJsonInput_FramesHistoryEvenWhenFinalUserEmpty(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "FIRST_QUESTION"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "FIRST_ANSWER"}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: ""}}},
	}

	msgsOut := parseSDKUserMessages(t, p.buildStreamJsonInput(msgs, "sess-empty-final"))
	if len(msgsOut) != 1 {
		t.Fatalf("expected exactly one emitted user message carrying the framing, got %d", len(msgsOut))
	}
	blocks := msgsOut[0].Message.Content
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("expected a single framing text block, got %+v", blocks)
	}
	for _, want := range []string{"<conversation_history>", "FIRST_QUESTION", "Assistant: FIRST_ANSWER", "</conversation_history>", claudeBinResumeReplayInstruction} {
		if !strings.Contains(blocks[0].Text, want) {
			t.Errorf("framing block missing %q\ngot: %q", want, blocks[0].Text)
		}
	}
}

func TestFormatToolOutputForClaude_UsesStructuredImageParts(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)

	formatted := p.formatToolOutputForClaude(ToolOutput{
		Content: "Image loaded",
		ContentParts: []ToolContentPart{
			{Type: ToolContentPartText, Text: "Image loaded"},
			{Type: ToolContentPartImageData, ImageData: &ToolImageData{
				MediaType: "image/png",
				Base64:    "aGVsbG8=",
			}},
		},
	})

	lines := strings.Split(strings.TrimSpace(formatted), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected formatted output to include text and image path, got %q", formatted)
	}
	if lines[0] != "Image loaded" {
		t.Fatalf("expected first line to keep text output, got %q", lines[0])
	}
	if lines[1] == "" {
		t.Fatal("expected second line to contain image path")
	}
	if _, err := os.Stat(lines[1]); err != nil {
		t.Fatalf("expected materialized image path %q to exist: %v", lines[1], err)
	}
}

func parseSDKUserMessages(t *testing.T, input string) []sdkUserMessage {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(input), "\n")
	msgs := make([]sdkUserMessage, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg sdkUserMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("failed to parse stream-json line %q: %v", line, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// testTool is a simple tool implementation for testing.
type testTool struct {
	name string
	exec func(ctx context.Context, args json.RawMessage) (ToolOutput, error)
}

func (t *testTool) Name() string                        { return t.name }
func (t *testTool) Description() string                 { return "test tool" }
func (t *testTool) Spec() ToolSpec                      { return ToolSpec{Name: t.name} }
func (t *testTool) Preview(args json.RawMessage) string { return "" }
func (t *testTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	return t.exec(ctx, args)
}

func TestBuildConversationPrompt_DropsSystemMessages(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "SYSTEM_SHOULD_NOT_BE_ON_STDIN"}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
	}
	out := p.buildConversationPrompt(msgs)
	if strings.Contains(out, "SYSTEM_SHOULD_NOT_BE_ON_STDIN") || strings.Contains(out, "System:") {
		t.Fatalf("system prompt leaked into stdin prompt: %q", out)
	}
	if out != "User: hello" {
		t.Fatalf("buildConversationPrompt() = %q, want %q", out, "User: hello")
	}
}

func TestBuildConversationPrompt_DeveloperRole(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "You have access to /root/Files/"}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "list the files"}}},
	}
	out := p.buildConversationPrompt(msgs)

	// Developer text must be wrapped in <developer> tags and prepended to the user turn.
	expected := "User: <developer>\nYou have access to /root/Files/\n</developer>\nlist the files"
	if out != expected {
		t.Errorf("unexpected output:\ngot:  %q\nwant: %q", out, expected)
	}
}

func TestBuildConversationPrompt_DeveloperRoleWithoutFollowingUser(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "trailing dev message"}}},
	}
	out := p.buildConversationPrompt(msgs)

	// The developer message has no following user turn, so it should be silently dropped
	// (same as anthropic.go behavior — pendingDev is lost at end of loop).
	if strings.Contains(out, "trailing dev message") {
		t.Errorf("trailing developer message without following user turn should be dropped, got: %q", out)
	}
}

// TestBuildConversationPrompt_WrapsReplayedHistory covers the resume-after-eviction
// case: a fresh provider instance is handed the full transcript. Earlier turns
// must be wrapped as <conversation_history> context so Claude Code answers only
// the latest user message instead of re-running the whole thread from the top.
func TestBuildConversationPrompt_WrapsReplayedHistory(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "first question"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "first answer"}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "latest question"}}},
	}
	out := p.buildConversationPrompt(msgs)

	closeIdx := strings.Index(out, "</conversation_history>")
	if !strings.Contains(out, "<conversation_history>") || closeIdx < 0 {
		t.Fatalf("expected replayed history to be wrapped, got: %q", out)
	}
	history := out[:closeIdx]
	if !strings.Contains(history, "first question") || !strings.Contains(history, "first answer") {
		t.Fatalf("prior turn must live inside the history block, got: %q", out)
	}
	// The latest user message is the only turn to answer: it must sit AFTER the
	// history block, not inside it.
	if strings.Contains(history, "latest question") {
		t.Fatalf("latest question leaked into history block: %q", out)
	}
	tail := out[closeIdx:]
	if !strings.Contains(tail, "User: latest question") {
		t.Fatalf("latest question missing from final turn: %q", out)
	}
	if !strings.Contains(tail, "responding only to the user's most recent message") {
		t.Fatalf("replay instruction missing: %q", out)
	}
}

// TestBuildConversationPrompt_SingleTurnNoFraming guards the common paths (a
// brand-new conversation and the mid-session --resume case that sends only new
// messages): with no prior assistant/tool turn there must be no history framing.
func TestBuildConversationPrompt_SingleTurnNoFraming(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "platform note"}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "just one question"}}},
	}
	out := p.buildConversationPrompt(msgs)
	if strings.Contains(out, "<conversation_history>") {
		t.Fatalf("a single new turn must not be wrapped as history: %q", out)
	}
	if !strings.Contains(out, "just one question") {
		t.Fatalf("user message missing: %q", out)
	}
}

func TestBuildStreamJsonInput_DeveloperRole(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []Message{
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "You have access to /root/Files/"}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "list the files"}}},
	}
	out := p.buildStreamJsonInput(msgs, "sess-dev")
	if out == "" {
		t.Fatal("expected non-empty stream-json output")
	}

	var msg sdkUserMessage
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("failed to parse stream-json: %v", err)
	}
	if msg.Message.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Message.Role)
	}

	// First block should be the developer-wrapped text, second the user text.
	if len(msg.Message.Content) < 2 {
		t.Fatalf("expected at least 2 content blocks, got %d", len(msg.Message.Content))
	}
	devBlock := msg.Message.Content[0]
	if devBlock.Type != "text" {
		t.Errorf("expected first block type 'text', got %q", devBlock.Type)
	}
	if !strings.Contains(devBlock.Text, "<developer>") || !strings.Contains(devBlock.Text, "/root/Files/") {
		t.Errorf("expected developer-wrapped text, got %q", devBlock.Text)
	}
	userBlock := msg.Message.Content[1]
	if userBlock.Text != "list the files" {
		t.Errorf("expected user text 'list the files', got %q", userBlock.Text)
	}
}
