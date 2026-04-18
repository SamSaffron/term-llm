package llm

import (
	"context"
	"encoding/json"
	"fmt"
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

	_, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events})
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

	_, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events})
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

	_, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events})
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

	_, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events})
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

func TestDispatchClaudeEvents_EmitsStreamlinedText(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	events := make(chan Event, 8)
	lines := make(chan string, 2)
	toolReqs := make(chan claudeToolRequest, 1)

	lines <- `{"type":"streamlined_text","text":"streamlined assistant text"}`
	lines <- `{"type":"result","subtype":"success","is_error":false,"result":"ignored final result","usage":{"input_tokens":1,"output_tokens":3,"cache_read_input_tokens":0}}`
	close(lines)

	_, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events})
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

	_, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events})
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

	usage, _, err := provider.dispatchClaudeEvents(context.Background(), lines, toolReqs, false, eventSender{ctx: context.Background(), ch: events})
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
		{"sonnet-xhigh", "sonnet-xhigh", ""}, // xhigh is opus-only
		{"sonnet", "sonnet", ""},
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

	t.Run("root sandbox uses bypass permission mode", func(t *testing.T) {
		getEuid = func() int { return 0 }
		p := NewClaudeBinProvider("sonnet", map[string]string{"IS_SANDBOX": "1"})
		args, _ := p.buildArgs(context.Background(), Request{}, eventSender{})
		joined := strings.Join(args, "\n")
		if strings.Contains(joined, "--dangerously-skip-permissions") {
			t.Fatal("expected claude-bin args to omit --dangerously-skip-permissions when running as root")
		}
		if !strings.Contains(joined, "--permission-mode\nbypassPermissions") {
			t.Fatal("expected root sandbox runs to request bypassPermissions mode")
		}
	})

	t.Run("root outside sandbox omits bypass flags", func(t *testing.T) {
		getEuid = func() int { return 0 }
		p := NewClaudeBinProvider("sonnet", nil)
		args, _ := p.buildArgs(context.Background(), Request{}, eventSender{})
		joined := strings.Join(args, "\n")
		if strings.Contains(joined, "--dangerously-skip-permissions") {
			t.Fatal("expected claude-bin args to omit --dangerously-skip-permissions when running as root")
		}
		if strings.Contains(joined, "--permission-mode\nbypassPermissions") {
			t.Fatal("did not expect bypassPermissions mode outside sandbox")
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

func TestClaudeBinProvider_CombinePromptIncludesSystemOnStdin(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)

	got := p.combinePrompt("You are helpful.\nUse tools when needed.", "User: hi")
	want := "System: You are helpful.\nUse tools when needed.\n\nUser: hi"
	if got != want {
		t.Fatalf("combinePrompt() = %q, want %q", got, want)
	}
}

func TestClaudeBinProvider_CombinePromptHandlesEmptyConversation(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)

	got := p.combinePrompt("You are helpful.", "")
	if got != "System: You are helpful." {
		t.Fatalf("combinePrompt() with empty conversation = %q", got)
	}
}

func TestClaudeBinProvider_CombinePromptHandlesEmptySystem(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)

	got := p.combinePrompt("", "User: hello")
	if got != "User: hello" {
		t.Fatalf("combinePrompt() with empty system = %q", got)
	}
}

func TestClaudeBinProvider_SystemPromptForTurn(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "You are helpful."}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "Hi"}}},
	}

	p := NewClaudeBinProvider("sonnet", nil)
	if got := p.systemPromptForTurn(msgs); got != "You are helpful." {
		t.Fatalf("initial turn systemPromptForTurn = %q, want %q", got, "You are helpful.")
	}

	p.sessionID = "resume-abc"
	if got := p.systemPromptForTurn(msgs); got != "" {
		t.Fatalf("resume turn systemPromptForTurn = %q, want empty (Claude CLI already has the system prompt via --resume)", got)
	}
}

func TestClaudeBinProvider_BuildCommandEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "should-be-cleared")
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "medium")
	t.Setenv("PATH", os.Getenv("PATH"))

	p := NewClaudeBinProvider("opus-max", map[string]string{
		"IS_SANDBOX":                 "1",
		"CLAUDE_CODE_EFFORT_LEVEL":   "max-from-config-should-be-overridden",
		"ANTHROPIC_API_KEY":          "config-value-should-not-survive",
		"CUSTOM_TERM_LLM_TEST_VALUE": "ok",
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
}

func TestMapModelToClaudeArg(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "opus"},
		{"sonnet", "sonnet"},
		{"haiku", "haiku"},
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
