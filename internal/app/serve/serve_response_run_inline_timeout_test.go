package serve

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

// Inline-loop providers (claude-bin and the other CLI bridges) run their whole
// tool loop in one provider stream. A loop that keeps calling tools must not
// time out just because the stream as a whole outlasts the inactivity window.
func TestStartResponseRunInlineToolLoopRefreshesTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	var calls []core.ToolCall
	for _, id := range []string{"call-1", "call-2", "call-3", "call-4"} {
		calls = append(calls, core.ToolCall{ID: id, Name: "response_timeout_delay", Arguments: json.RawMessage(`{}`)})
	}
	provider := mockprovider.NewMockProvider("mock").
		WithCapabilities(core.Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: true}).
		AddTurn(mockprovider.MockTurn{ToolCalls: calls, InlineTools: true, InlineText: "finished"})
	registry := engine.NewToolRegistry()
	// Each call fits the window, but the four together outlast it.
	registry.Register(responseTimeoutDelayTool{delay: 80 * time.Millisecond})
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine.NewEngine(provider, registry),
		defaultModel: "mock-model",
	}
	rt.Touch()
	srv := &serveServer{
		cfg:          serveServerConfig{responseTimeout: timeout},
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	run, err := srv.startResponseRun(rt, true, false, []core.Message{core.UserText("work")}, core.Request{
		SessionID: "sess_inline_refresh_timeout",
		Tools:     []core.ToolSpec{responseTimeoutDelayTool{}.Spec()},
	}, "sess_inline_refresh_timeout", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun: %v", err)
	}
	waitForServeCondition(t, 2*time.Second, func() bool {
		status := stringValue(run.snapshot()["status"])
		if status == "failed" {
			t.Fatalf("inline tool loop failed: %v", run.snapshot()["error"])
		}
		return status == "completed"
	}, "inline tool loop to complete across refreshed timeouts")
	for _, call := range calls {
		if _, ok := provider.InlineToolResult(call.ID); !ok {
			t.Fatalf("inline call %s never received a result", call.ID)
		}
	}
}

func TestAppendResponseToolCallRefreshesTimeoutOnlyForInlineCalls(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inline      bool
		wantRefresh bool
	}{
		{name: "inline call", inline: true, wantRefresh: true},
		// Engine-run calls follow a completed response, which already refreshed.
		{name: "engine call", inline: false, wantRefresh: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeResponseRunClock()
			ctx, timer := newResponseRunTimerWithClock(time.Minute, clock)
			defer timer.stop()
			run := newResponseRun("resp_inline_refresh", "sess_test", "", "mock", clock.Now().Unix(), func() {})
			runtime := &serveRuntime{refreshResponseTimeout: timer.refresh}
			state := newResponseRunStreamState("mock", "")

			clock.Advance(50 * time.Second)
			if err := (&serveServer{}).appendResponseRunEvent(runtime, run, state, core.Event{
				Type:       core.EventToolCall,
				ToolCallID: "call-1",
				ToolName:   "shell",
				Tool:       &core.ToolCall{ID: "call-1", Name: "shell"},
				ToolInline: tc.inline,
			}); err != nil {
				t.Fatal(err)
			}
			clock.Advance(50 * time.Second)
			if refreshed := ctx.Err() == nil; refreshed != tc.wantRefresh {
				t.Fatalf("window refreshed = %v, want %v (cause %v)", refreshed, tc.wantRefresh, context.Cause(ctx))
			}
			if !tc.wantRefresh {
				return
			}
			// The fresh window still bounds a loop that goes quiet.
			clock.Advance(11 * time.Second)
			if !responseRunTimedOut(ctx) {
				t.Fatalf("timer cause = %v, want response timeout", context.Cause(ctx))
			}
		})
	}
}
