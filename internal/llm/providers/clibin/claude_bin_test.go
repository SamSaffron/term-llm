package clibin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/mcphttp"
)

func TestClaudeBinProvider_ImplementsToolExecutorSetter(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)

	// This type assertion must succeed for tools to work.
	// The bug was that ClaudeBinProvider.SetToolExecutor used mcphttp.ToolExecutor
	// (a named type) instead of the anonymous function type in the interface,
	// which caused this assertion to fail silently.
	if _, ok := interface{}(provider).(toolExecutorSetter); !ok {
		t.Fatal("ClaudeBinProvider does not implement toolExecutorSetter interface - tools will not work")
	}
}

func TestClaudeBinProvider_NativeInterruptEndsActiveProseTurn(t *testing.T) {
	binDir := t.TempDir()
	readyFile := filepath.Join(t.TempDir(), "ready")
	script := `#!/usr/bin/env python3
import json, os, sys
first = json.loads(sys.stdin.readline())
if first.get("type") != "user":
    sys.exit(2)
open(os.environ["CLAUDE_INTERRUPT_READY"], "w").close()
control = json.loads(sys.stdin.readline())
if control.get("type") != "control_request" or control.get("request", {}).get("subtype") != "interrupt":
    sys.exit(3)
print(json.dumps({"type":"control_response","response":{"subtype":"success","request_id":control.get("request_id"),"response":{"still_queued":[]}}}), flush=True)
print(json.dumps({"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}}), flush=True)
print(json.dumps({"type":"result","subtype":"error_during_execution","is_error":True,"result":"interrupted by user","usage":{}}), flush=True)
sys.exit(1)
`
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_INTERRUPT_READY", readyFile)

	provider := NewClaudeBinProvider("haiku", nil)
	stream, err := provider.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("long answer")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	type streamResult struct {
		text string
		err  error
	}
	done := make(chan streamResult, 1)
	go func() {
		var text strings.Builder
		for {
			event, recvErr := stream.Recv()
			if recvErr == io.EOF {
				done <- streamResult{text: text.String()}
				return
			}
			if recvErr != nil {
				done <- streamResult{err: recvErr}
				return
			}
			if event.Type == core.EventTextDelta {
				text.WriteString(event.Text)
			}
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(readyFile); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake Claude did not receive initial stream-json user message")
		}
		time.Sleep(10 * time.Millisecond)
	}
	provider.RequestInlineFlush()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("interrupted stream failed: %v", result.err)
		}
		if result.text != "partial" {
			t.Fatalf("stream text = %q, want partial output preserved", result.text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native interrupt did not end Claude stream")
	}
	if provider.inlineFlushRequested() {
		t.Fatal("successful native interrupt retained tool-boundary fallback")
	}
}

func TestClaudeBinProvider_ImplementsProviderCleaner(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)

	// ClaudeBinProvider must implement providerCleaner for MCP server cleanup
	if _, ok := interface{}(provider).(providerCleaner); !ok {
		t.Fatal("ClaudeBinProvider does not implement providerCleaner interface - MCP server cleanup will not work")
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

	if _, ok := interface{}(provider).(providerTurnCleaner); !ok {
		t.Fatal("ClaudeBinProvider does not implement providerTurnCleaner - per-turn temp file cleanup will not run")
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

func TestClaudeBinProvider_prepareClaudeCommand_ConfiguresProcessGroupWaitDelayAndWorkingDir(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	// Production code cleans paths; TMPDIR may end in "/" (macOS).
	workingDir := filepath.Clean(t.TempDir())

	cmd, stdin, cleanup, err := provider.prepareClaudeCommand(context.Background(), []string{"--version"}, "", workingDir)
	if err != nil {
		t.Fatalf("prepareClaudeCommand failed: %v", err)
	}
	defer cleanup()
	defer stdin.Close()

	if cmd.Dir != workingDir {
		t.Fatalf("Dir = %q, want %q", cmd.Dir, workingDir)
	}
	if cmd.WaitDelay != claudeCommandWaitDelay {
		t.Fatalf("WaitDelay = %v, want %v", cmd.WaitDelay, claudeCommandWaitDelay)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("expected claude subprocess to run in a detached session")
	}
	if cmd.Cancel == nil {
		t.Fatal("expected claude subprocess to install process-group cancellation")
	}
}

func TestClaudeBinProvider_OversizedStderrLineDoesNotBlock(t *testing.T) {
	err := runFakeClaudeCommand(t, `#!/bin/sh
IFS= read -r _
head -c 3145728 /dev/zero | tr '\000' x >&2
printf '%s\n' '{"type":"system","session_id":"test-session","model":"sonnet","tools":[]}'
printf '%s\n' '{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0}}'
`)
	if err != nil {
		t.Fatalf("runClaudeCommand: %v", err)
	}
}

func TestClaudeBinProvider_OversizedStdoutRecordCancelsProcess(t *testing.T) {
	err := runFakeClaudeCommand(t, `#!/bin/sh
IFS= read -r _
head -c 12582912 /dev/zero | tr '\000' x
`)
	if err == nil || !strings.Contains(err.Error(), "reading claude output") {
		t.Fatalf("runClaudeCommand error = %v, want stdout read error", err)
	}
}

func runFakeClaudeCommand(t *testing.T, script string) error {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan error, 1)
	go func() {
		provider := NewClaudeBinProvider("sonnet", nil)
		resultCh <- provider.runClaudeCommand(ctx, nil, "", "prompt", "", false, eventSender{
			ctx: ctx,
			ch:  make(chan core.Event, 8),
		}, true, false)
	}()

	select {
	case err := <-resultCh:
		return err
	case <-time.After(5 * time.Second):
		cancel()
		select {
		case <-resultCh:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("fake Claude command remained blocked on an oversized pipe record")
		return nil
	}
}

func TestClaudeBinProvider_StreamUsesWorkingDir(t *testing.T) {
	binDir := t.TempDir()
	cwdFile := filepath.Join(t.TempDir(), "cwd.txt")
	script := `#!/bin/sh
pwd > "$CLAUDE_TEST_CWD_FILE"
while IFS= read -r _; do :; done
printf '%s\n' '{"type":"system","session_id":"test-session","model":"sonnet","tools":[]}'
printf '%s\n' '{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0}}'
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_TEST_CWD_FILE", cwdFile)

	workingDir := t.TempDir()
	provider := NewClaudeBinProvider("sonnet", nil)
	provider.commandRunner = func(_ context.Context, _ []string, _, _, workingDir string, _ bool, _ eventSender, _, _ bool) error {
		return os.WriteFile(cwdFile, []byte(workingDir+"\n"), 0o644)
	}
	stream, err := provider.Stream(context.Background(), core.Request{
		Ephemeral:  true,
		WorkingDir: workingDir,
		Messages:   []core.Message{core.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == core.EventError {
			t.Fatalf("stream error: %v", event.Err)
		}
	}

	captured, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatalf("read captured cwd: %v", err)
	}
	gotCWD := strings.TrimSpace(string(captured))
	gotInfo, gotErr := os.Stat(gotCWD)
	wantInfo, wantErr := os.Stat(workingDir)
	if gotErr != nil || wantErr != nil || !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("Claude process cwd = %q, want %q (stat errors: %v, %v)", gotCWD, workingDir, gotErr, wantErr)
	}
}

func TestSafeSendEvent_ClosedChannel(t *testing.T) {
	// Test that safeSendEvent doesn't panic on closed channel
	ch := make(chan core.Event)
	close(ch)

	ctx := context.Background()
	event := core.Event{Type: core.EventTextDelta, Text: "test"}

	// Should not panic and should return false
	sent := safeSendEvent(ctx, ch, event)
	if sent {
		t.Fatal("safeSendEvent should return false for closed channel")
	}
}

func TestSafeSendEvent_CancelledContext(t *testing.T) {
	// Test that safeSendEvent respects context cancellation
	ch := make(chan core.Event) // unbuffered, will block

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	event := core.Event{Type: core.EventTextDelta, Text: "test"}

	// Should return false due to cancelled context
	sent := safeSendEvent(ctx, ch, event)
	if sent {
		t.Fatal("safeSendEvent should return false for cancelled context")
	}
}

func TestSafeSendEvent_Success(t *testing.T) {
	// Test that safeSendEvent works normally with open channel
	ch := make(chan core.Event, 1) // buffered so we don't block

	ctx := context.Background()
	event := core.Event{Type: core.EventTextDelta, Text: "test"}

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
	events := make(chan core.Event, 8)
	lines := make(chan string, 4)
	toolReqs := make(chan claudeToolRequest, 2)

	lines <- `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"text-before-tool"}}}`
	req := claudeToolRequest{
		ctx:      context.Background(),
		callID:   "call-1",
		name:     "read_file",
		args:     json.RawMessage(`{"path":"README.md"}`),
		response: make(chan core.ToolExecutionResponse, 1),
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
	if first.Type != core.EventTextDelta || first.Text != "text-before-tool" {
		t.Fatalf("expected first event to be text delta, got %+v", first)
	}

	second := <-events
	if second.Type != core.EventToolCall || second.ToolName != "read_file" {
		t.Fatalf("expected second event to be tool call, got %+v", second)
	}
}

func TestDispatchClaudeEvents_PrioritizesSlightlyDelayedTextOverToolRequest(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan core.Event, 8)
	lines := make(chan string, 4)
	toolReqs := make(chan claudeToolRequest, 2)

	req := claudeToolRequest{
		ctx:      context.Background(),
		callID:   "call-2",
		name:     "set_commit_message",
		args:     json.RawMessage(`{"message":"m"}`),
		response: make(chan core.ToolExecutionResponse, 1),
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
	if first.Type != core.EventTextDelta || first.Text != "tail-text" {
		t.Fatalf("expected first event to be delayed text delta, got %+v", first)
	}

	second := <-events
	if second.Type != core.EventToolCall || second.ToolName != "set_commit_message" {
		t.Fatalf("expected second event to be tool call, got %+v", second)
	}
}

func TestDispatchClaudeEvents_FallsBackToAssistantTextWhenNoDeltas(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan core.Event, 8)
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
		if ev.Type != core.EventTextDelta {
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
	events := make(chan core.Event, 8)
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
		if ev.Type != core.EventTextDelta {
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
	events := make(chan core.Event, 8)
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
		if ev.Type != core.EventTextDelta {
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
	events := make(chan core.Event, 8)
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
	events := make(chan core.Event, 8)
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
		if ev.Type != core.EventTextDelta {
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
	events := make(chan core.Event, 8)
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

	var got []core.Event
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
	if got[0].Type != core.EventTextDelta || got[0].Text != "delta text" {
		t.Fatalf("unexpected event: %+v", got[0])
	}
}

func TestDispatchClaudeEvents_SeparatesCachedInputTokensInUsage(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan core.Event, 8)
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
	events := make(chan core.Event)
	close(events)

	req := claudeToolRequest{
		ctx:      context.Background(),
		callID:   "call-1",
		name:     "read_file",
		args:     json.RawMessage(`{"path":"README.md"}`),
		response: make(chan core.ToolExecutionResponse, 1),
		ack:      make(chan error, 1),
	}

	provider.handleClaudeToolRequest(req, eventSender{ctx: context.Background(), ch: events})
	if err := <-req.ack; err == nil {
		t.Fatal("expected closed stream error")
	}
}

func TestHandleClaudeLine_ContextCancelledDoesNotBlock(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan core.Event) // unbuffered/no receiver to simulate blocked sink
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var usage *core.Usage
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

	args, _ := p.buildArgs(context.Background(), core.Request{}, eventSender{})
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
		args, _ := p.buildArgs(context.Background(), core.Request{}, eventSender{})
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
		args, _ := p.buildArgs(context.Background(), core.Request{}, eventSender{})
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

	args, _ := p.buildArgs(context.Background(), core.Request{}, eventSender{})
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
			_, effort := p.buildArgs(context.Background(), core.Request{
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

	args, _ := p.buildArgs(context.Background(), core.Request{
		Tools: []core.ToolSpec{{Name: "diagnostic_ping", Description: "diagnostic"}},
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
	msgs := []core.Message{
		{Role: core.RoleSystem, Parts: []core.Part{{Type: core.PartText, Text: "You are helpful."}}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "Hi"}}},
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
while IFS= read -r line || [ -n "$line" ]; do printf '%s\n' "$line"; done > "$CLAUDE_STDIN_FILE"
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
	p.commandRunner = func(_ context.Context, args []string, _, prompt, _ string, _ bool, send eventSender, _, _ bool) error {
		if err := os.WriteFile(argsFile, []byte(strings.Join(args, "\n")+"\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(stdinFile, []byte(prompt), 0o644); err != nil {
			return err
		}
		return send.Send(core.Event{Type: core.EventTextDelta, Text: "ok"})
	}
	p.sessionID = "chat-session"
	p.messagesSent = 7

	stream, err := p.Stream(context.Background(), core.Request{
		Ephemeral: true,
		Messages: []core.Message{
			core.SystemText("guardian policy"),
			core.UserText("review this command"),
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
while [ ! -f "$CLAUDE_RELEASE_FILE" ]; do :; done
printf '%s\n' '{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0}}'
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_READY_FILE", readyFile)
	t.Setenv("CLAUDE_RELEASE_FILE", releaseFile)

	p := NewClaudeBinProvider("sonnet", nil)
	p.commandRunner = func(ctx context.Context, _ []string, _, _, _ string, _ bool, send eventSender, _, expose bool) error {
		bridge := &claudeTurnBridge{toolReqCh: make(chan claudeToolRequest, 1), done: make(chan struct{})}
		if expose {
			p.eventsMu.Lock()
			p.currentBridge = bridge
			p.currentEvents = send.ch
			p.eventsMu.Unlock()
		}
		if err := os.WriteFile(readyFile, nil, 0o644); err != nil {
			return err
		}
		defer func() {
			p.eventsMu.Lock()
			p.currentBridge = nil
			p.currentEvents = nil
			p.eventsMu.Unlock()
		}()
		for {
			if _, err := os.Stat(releaseFile); err == nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	}
	stream, err := p.Stream(context.Background(), core.Request{
		Ephemeral: true,
		Messages:  []core.Message{core.UserText("standalone tool request")},
		Tools:     []core.ToolSpec{{Name: "diagnostic_ping", Description: "diagnostic"}},
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
	var events chan<- core.Event
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
	stream, err := p.Stream(context.Background(), core.Request{
		Ephemeral: true,
		Messages:  []core.Message{core.UserText("standalone tool request")},
		Tools:     []core.ToolSpec{{Name: "diagnostic_ping", Description: "diagnostic"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Type != core.EventError || ev.Err == nil || !strings.Contains(ev.Err.Error(), "ephemeral tool request cannot run") {
		t.Fatalf("Recv() = %#v, want ephemeral tool active-stream error", ev)
	}
}

func TestClaudeBinProvider_EphemeralSystemLineDoesNotReplaceSession(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	p.sessionID = "chat-session"
	var usage *core.Usage
	sawTextDelta := false
	assistantFallbackText := ""
	if err := p.handleClaudeLine(
		context.Background(),
		`{"type":"system","session_id":"guardian-session","model":"sonnet","tools":[]}`,
		false,
		eventSender{ctx: context.Background(), ch: make(chan core.Event, 1)},
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

func TestClaudeBinProviderParsesCacheCreationInputTokens(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	var usage *core.Usage
	sawText := false
	fallback := ""
	handled := false
	events := make(chan core.Event, 1)
	err := provider.handleClaudeLine(
		context.Background(),
		`{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":12,"output_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":127000}}`,
		false,
		eventSender{ctx: context.Background(), ch: events},
		&usage,
		&sawText,
		&fallback,
		&handled,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.InputTokens != 12 || usage.CachedInputTokens != 5 || usage.CacheWriteTokens != 127_000 {
		t.Fatalf("usage = %#v", usage)
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

	args, _ := p.buildArgs(context.Background(), core.Request{}, eventSender{})
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

	args, _ := restored.buildArgs(context.Background(), core.Request{}, eventSender{})
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
		"",
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
		"/tmp/worktree",
		false,
		[]string{`{"type":"system","subtype":"init"}`},
		[]string{"fatal problem"},
	)

	if !strings.Contains(err.Error(), "claude command failed (exit 7)") {
		t.Fatalf("unexpected error string: %s", err.Error())
	}
	fields := err.DebugFields()
	if got := fields["cwd"].(string); got != "/tmp/worktree" {
		t.Fatalf("cwd = %q, want /tmp/worktree", got)
	}
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

	err := p.newClaudeCommandError(fmt.Errorf("exit status 1"), 1, []string{"--print"}, "", longPrompt, "", false, []string{longLine}, []string{longLine})
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

	messages := []core.Message{
		{
			Role: core.RoleUser,
			Parts: []core.Part{
				{Type: core.PartText, Text: "hello"},
			},
		},
		{
			Role: core.RoleTool,
			Parts: []core.Part{
				{
					Type: core.PartToolResult,
					ToolResult: &core.ToolResult{
						ID:      "1",
						Name:    "short_tool",
						Content: shortContent,
					},
				},
				{
					Type: core.PartToolResult,
					ToolResult: &core.ToolResult{
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
	messages := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "hello"}}},
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, Text: "hi"}}},
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
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{
			{Type: core.PartText, Text: "look at this"},
			{Type: core.PartImage, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
		}},
	}
	if !hasImages(msgs) {
		t.Fatal("expected hasImages to return true for user PartImage")
	}
}

func TestHasImages_ToolResultImage(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleTool, Parts: []core.Part{
			{Type: core.PartToolResult, ToolResult: &core.ToolResult{
				ID:   "1",
				Name: "screenshot",
				ContentParts: []core.ToolContentPart{
					{Type: core.ToolContentPartText, Text: "here is the image"},
					{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
				},
			}},
		}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "what do you see?"}}},
	}
	if !hasImages(msgs) {
		t.Fatal("expected hasImages to return true for tool result image")
	}
}

func TestHasImages_TextOnly(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "hello"}}},
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, Text: "hi"}}},
		{Role: core.RoleTool, Parts: []core.Part{
			{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: "1", Name: "t", Content: "result"}},
		}},
	}
	if hasImages(msgs) {
		t.Fatal("expected hasImages to return false for text-only messages")
	}
}

func TestBuildStreamJsonInput_UserPastedImage(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{
			{Type: core.PartImage, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: core.PartText, Text: "describe this"},
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
	msgs := []core.Message{
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, Text: "let me inspect that image"}}},
		{Role: core.RoleTool, Parts: []core.Part{
			{Type: core.PartToolResult, ToolResult: &core.ToolResult{
				ID:   "tool-123",
				Name: "view_image",
				ContentParts: []core.ToolContentPart{
					{Type: core.ToolContentPartText, Text: "Image loaded"},
					{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
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
	msgs := []core.Message{
		{Role: core.RoleTool, Parts: []core.Part{
			{Type: core.PartToolResult, ToolResult: &core.ToolResult{
				ID:   "tool-456",
				Name: "view_image",
				ContentParts: []core.ToolContentPart{
					{Type: core.ToolContentPartText, Text: "Image loaded"},
					{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
						MediaType: "image/png",
						Base64:    "aGVsbG8=",
					}},
				},
			}},
		}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "what is shown?"}}},
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
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "hello"}}},
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
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{
			{Type: core.PartImage, ImageData: &core.ToolImageData{MediaType: "image/jpeg", Base64: "dGVzdA=="}},
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
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "FIRST_QUESTION"}}},
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, Text: "FIRST_ANSWER"}}},
		{Role: core.RoleUser, Parts: []core.Part{
			{Type: core.PartText, Text: "LATEST_QUESTION"},
			{Type: core.PartImage, ImageData: &core.ToolImageData{MediaType: "image/jpeg", Base64: "dGVzdA=="}},
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
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{
			{Type: core.PartText, Text: "hello"},
			{Type: core.PartImage, ImageData: &core.ToolImageData{MediaType: "image/jpeg", Base64: "dGVzdA=="}},
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
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "FIRST_QUESTION"}}},
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, Text: "FIRST_ANSWER"}}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: ""}}},
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

func TestFormatToolResultForClaude_EmitsImageParts(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)

	result := p.formatToolResultForClaude(testClaudeImageToolOutput())

	if result.Content != "Image loaded" {
		t.Fatalf("Content = %q, want %q", result.Content, "Image loaded")
	}
	if len(result.Parts) != 2 {
		t.Fatalf("Parts = %#v, want text and image blocks", result.Parts)
	}
	if result.Parts[0].Type != mcphttp.ContentPartText || result.Parts[0].Text != "Image loaded" {
		t.Errorf("text part = %#v", result.Parts[0])
	}
	if image := result.Parts[1]; image.Type != mcphttp.ContentPartImage || image.MIMEType != "image/png" || string(image.Data) != "hello" {
		t.Errorf("image part = %#v", image)
	}

	p.tempFilesMu.Lock()
	trackedFiles := len(p.tempFiles)
	p.tempFilesMu.Unlock()
	if trackedFiles != 0 {
		t.Fatalf("formatting tracked %d temp files, want none", trackedFiles)
	}
}

func TestFormatToolResultForClaude_TextOnlyHasNoParts(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)

	result := p.formatToolResultForClaude(core.TextOutput("tool ok"))

	if result.Content != "tool ok" {
		t.Fatalf("Content = %q, want %q", result.Content, "tool ok")
	}
	if result.Parts != nil {
		t.Fatalf("Parts = %#v, want nil", result.Parts)
	}
}

func TestFormatToolResultForClaude_FlushNoticeStaysInTextPart(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	p.requestInlineFlush()

	result := p.formatToolResultForClaude(testClaudeImageToolOutput())

	if len(result.Parts) != 2 {
		t.Fatalf("Parts = %#v, want text and image blocks", result.Parts)
	}
	if result.Parts[0].Type != mcphttp.ContentPartText || !strings.HasSuffix(result.Parts[0].Text, inlineFlushToolNotice) {
		t.Fatalf("text part = %#v, want inline flush notice", result.Parts[0])
	}
	if result.Parts[1].Type != mcphttp.ContentPartImage {
		t.Fatalf("image part = %#v", result.Parts[1])
	}

	second := p.formatToolResultForClaude(testClaudeImageToolOutput())
	if strings.Contains(second.Content, strings.TrimSpace(inlineFlushToolNotice)) {
		t.Fatalf("second Content retained one-shot flush notice: %q", second.Content)
	}
}

func TestClaudeBinProvider_SetToolExecutor_EmitsImageParts(t *testing.T) {
	tests := []struct {
		name      string
		timedOut  bool
		wantError bool
	}{
		{name: "success"},
		{name: "timeout", timedOut: true, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := NewClaudeBinProvider("sonnet", nil)
			provider.SetToolExecutor(func(context.Context, string, json.RawMessage) (core.ToolOutput, error) {
				output := testClaudeImageToolOutput()
				output.TimedOut = tt.timedOut
				return output, nil
			})

			result, err := provider.toolExecutor(context.Background(), "view_image", json.RawMessage("{}"))
			if err != nil {
				t.Fatalf("toolExecutor: %v", err)
			}
			if result.IsError != tt.wantError {
				t.Errorf("IsError = %v, want %v", result.IsError, tt.wantError)
			}
			if len(result.Parts) != 2 || result.Parts[1].Type != mcphttp.ContentPartImage {
				t.Fatalf("Parts = %#v, want text and image blocks", result.Parts)
			}
		})
	}
}

func testClaudeImageToolOutput() core.ToolOutput {
	return core.ToolOutput{
		Content: "Image loaded",
		ContentParts: []core.ToolContentPart{
			{Type: core.ToolContentPartText, Text: "Image loaded"},
			{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
				MediaType: "image/png",
				Base64:    "aGVsbG8=",
			}},
		},
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
	exec func(ctx context.Context, args json.RawMessage) (core.ToolOutput, error)
}

func (t *testTool) Name() string                        { return t.name }
func (t *testTool) Description() string                 { return "test tool" }
func (t *testTool) Spec() core.ToolSpec                 { return core.ToolSpec{Name: t.name} }
func (t *testTool) Preview(args json.RawMessage) string { return "" }
func (t *testTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	return t.exec(ctx, args)
}

func TestBuildConversationPrompt_DropsSystemMessages(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []core.Message{
		{Role: core.RoleSystem, Parts: []core.Part{{Type: core.PartText, Text: "SYSTEM_SHOULD_NOT_BE_ON_STDIN"}}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "hello"}}},
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
	msgs := []core.Message{
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "Conversation started at a fixed time."}}},
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "You have access to /root/Files/"}}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "list the files"}}},
	}
	out := p.buildConversationPrompt(msgs)

	// Consecutive developer messages must share one wrapper before the user turn.
	expected := "User: <developer>\nConversation started at a fixed time.\n\nYou have access to /root/Files/\n</developer>\nlist the files"
	if out != expected {
		t.Errorf("unexpected output:\ngot:  %q\nwant: %q", out, expected)
	}
}

func TestBuildConversationPrompt_DeveloperRoleWithoutFollowingUser(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "hello"}}},
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "trailing dev message"}}},
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
	msgs := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "first question"}}},
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, Text: "first answer"}}},
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "platform transition"}}},
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "time anchor"}}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "latest question"}}},
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
	if !strings.Contains(tail, "User: <developer>") || !strings.Contains(tail, "platform transition") || !strings.Contains(tail, "time anchor") || !strings.Contains(tail, "latest question") {
		t.Fatalf("latest developer block and question missing from final turn: %q", out)
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
	msgs := []core.Message{
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "platform note"}}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "just one question"}}},
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
	msgs := []core.Message{
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "Conversation started at a fixed time."}}},
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "You have access to /root/Files/"}}},
		{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartText, Text: "list the files"}}},
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
	if !strings.Contains(devBlock.Text, "<developer>") || !strings.Contains(devBlock.Text, "Conversation started at a fixed time.") || !strings.Contains(devBlock.Text, "/root/Files/") {
		t.Errorf("expected both developer messages in wrapped text, got %q", devBlock.Text)
	}
	userBlock := msg.Message.Content[1]
	if userBlock.Text != "list the files" {
		t.Errorf("expected user text 'list the files', got %q", userBlock.Text)
	}
}

func TestClaudeBinProvider_ImplementsInlineFlusher(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	flusher, ok := interface{}(provider).(inlineFlusher)
	if !ok {
		t.Fatal("ClaudeBinProvider does not implement InlineFlusher")
	}
	if !flusher.SupportsInlineFlush() {
		t.Fatal("claude-bin does not report inline flush support")
	}
	interrupter, ok := interface{}(provider).(immediateInterrupter)
	if !ok || !interrupter.SupportsImmediateInterruption() {
		t.Fatal("claude-bin does not report immediate interruption support")
	}
	flusher.RequestInlineFlush()
	if !provider.inlineFlushRequested() {
		t.Fatal("RequestInlineFlush did not mark the tool-result boundary")
	}
	formatted := provider.formatToolResultForClaude(core.TextOutput("tool ok")).Content
	if !strings.Contains(formatted, strings.TrimSpace(inlineFlushToolNotice)) {
		t.Fatalf("formatToolResultForClaude content = %q, want flush notice", formatted)
	}
}

func TestHandoverClaudeBinForkSendsOnlyHelperSuffix(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	stdinFile := filepath.Join(dir, "stdin.txt")
	claudePath := filepath.Join(dir, "claude")
	const script = `#!/bin/sh
printf '%s\n' "$@" > "$CLAUDE_ARGS_FILE"
while IFS= read -r line || [ -n "$line" ]; do printf '%s\n' "$line"; done > "$CLAUDE_STDIN_FILE"
printf '%s\n' '{"type":"system","session_id":"forked-session","model":"sonnet","tools":[]}'
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"handover brief"}}}'
printf '%s\n' '{"type":"result","is_error":false,"result":"handover brief","usage":{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":100}}'
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_ARGS_FILE", argsFile)
	t.Setenv("CLAUDE_STDIN_FILE", stdinFile)

	live := NewClaudeBinProvider("sonnet", nil)
	live.commandRunner = func(_ context.Context, args []string, _, prompt, _ string, _ bool, send eventSender, _, _ bool) error {
		if err := os.WriteFile(argsFile, []byte(strings.Join(args, "\n")+"\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(stdinFile, []byte(prompt), 0o644); err != nil {
			return err
		}
		return send.Send(core.Event{Type: core.EventTextDelta, Text: "handover brief"})
	}
	live.sessionID = "live-session"
	live.messagesSent = 99 // Deliberately unrelated to the helper request shape.
	// Handover (root llm package) forks the live provider for its helper turn and
	// sends only the helper suffix; drive the same fork here.
	forked, ok := live.forkHelperConversation()
	if !ok {
		t.Fatal("expected a suffix-only handover fork")
	}
	const trigger = "write the handover brief"
	stream, err := forked.Stream(context.Background(), core.Request{
		Model: "sonnet",
		Messages: []core.Message{
			core.SystemText("system policy"),
			core.UserText("handing over"),
			core.UserText(trigger),
		},
		MaxTurns: 1,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var document strings.Builder
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		if event.Type == core.EventTextDelta {
			document.WriteString(event.Text)
		}
	}
	if document.String() != "handover brief" {
		t.Fatalf("document = %q", document.String())
	}

	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(argsBytes)
	if !strings.Contains(args, "--resume\nlive-session\n") || !strings.Contains(args, "--fork-session\n") {
		t.Fatalf("fork args missing resume/fork:\n%s", args)
	}
	stdinBytes, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	stdin := string(stdinBytes)
	if strings.Contains(stdin, "old conversation sentinel") || strings.Contains(stdin, "settled response") {
		t.Fatalf("fork stdin replayed parent transcript:\n%s", stdin)
	}
	for _, want := range []string{"handing over", trigger} {
		if !strings.Contains(stdin, want) {
			t.Fatalf("fork stdin missing %q:\n%s", want, stdin)
		}
	}
	if live.sessionID != "live-session" || live.messagesSent != 99 {
		t.Fatalf("live provider mutated: session=%q messages=%d", live.sessionID, live.messagesSent)
	}
}

func TestClaudeBinProvider_BuildCommandEnvIgnoresInteractiveOnlyShellEnv(t *testing.T) {
	origGetEuid := getEuid
	defer func() { getEuid = origGetEuid }()
	getEuid = func() int { return 1000 }

	// CLAUDE_CODE_SIMPLE skips OAuth ("Not logged in"), a shell effort level
	// would override term-llm's, and a shell MCP cap would undo the raised one.
	t.Setenv("CLAUDE_CODE_SIMPLE", "1")
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "low")
	t.Setenv("MAX_MCP_OUTPUT_TOKENS", "1000")
	t.Setenv("ENABLE_TOOL_SEARCH", "true")

	env := envSliceMap(NewClaudeBinProvider("opus", nil).buildCommandEnv(""))
	for _, key := range []string{"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_EFFORT_LEVEL"} {
		if got, ok := env[key]; ok {
			t.Fatalf("inherited %s survived as %q", key, got)
		}
	}
	if got := env["MAX_MCP_OUTPUT_TOKENS"]; got != claudeCodeDefaultEnv["MAX_MCP_OUTPUT_TOKENS"] {
		t.Fatalf("MAX_MCP_OUTPUT_TOKENS = %q, want default %q", got, claudeCodeDefaultEnv["MAX_MCP_OUTPUT_TOKENS"])
	}
	if got := env["ENABLE_TOOL_SEARCH"]; got != "false" {
		t.Fatalf("ENABLE_TOOL_SEARCH = %q, want forced false", got)
	}
}

func TestClaudeBinProvider_BuildCommandEnvHonoursExplicitProviderEnv(t *testing.T) {
	origGetEuid := getEuid
	defer func() { getEuid = origGetEuid }()
	getEuid = func() int { return 1000 }

	t.Setenv("MAX_MCP_OUTPUT_TOKENS", "1000")
	p := NewClaudeBinProvider("opus", map[string]string{
		"CLAUDE_CODE_SIMPLE":       "1",
		"CLAUDE_CODE_EFFORT_LEVEL": "high",
		"MAX_MCP_OUTPUT_TOKENS":    "50000",
	})
	env := envSliceMap(p.buildCommandEnv(""))
	want := map[string]string{
		"CLAUDE_CODE_SIMPLE":       "1",
		"CLAUDE_CODE_EFFORT_LEVEL": "high",
		"MAX_MCP_OUTPUT_TOKENS":    "50000",
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("%s = %q, want configured %q", key, env[key], value)
		}
	}
	if n := strings.Count(strings.Join(p.buildCommandEnv(""), "\n"), "MAX_MCP_OUTPUT_TOKENS="); n != 1 {
		t.Fatalf("MAX_MCP_OUTPUT_TOKENS appears %d times, want 1", n)
	}
}

func TestClaudeMCPToolSpecsPublishClaudeCodeHints(t *testing.T) {
	specs := claudeMCPToolSpecs([]core.ToolSpec{
		{Name: "shell", Schema: map[string]any{"type": "object"}},
		{Name: "read_file", Schema: map[string]any{"type": "object"}},
	})
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2", len(specs))
	}
	for _, spec := range specs {
		if spec.Meta["anthropic/alwaysLoad"] != true {
			t.Fatalf("%s alwaysLoad = %#v", spec.Name, spec.Meta)
		}
		if spec.Meta["anthropic/maxResultSizeChars"] != claudeMCPMaxResultSizeChars {
			t.Fatalf("%s maxResultSizeChars = %#v", spec.Name, spec.Meta)
		}
		if !spec.Parallelizable {
			t.Fatalf("%s lost the shared parallel hint", spec.Name)
		}
	}
}
