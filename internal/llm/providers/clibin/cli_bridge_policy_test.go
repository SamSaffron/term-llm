package clibin

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// The MCP bridge must enqueue an engine event, not call the legacy executor
// callback. Otherwise tool policy and the code-mode controller could be skipped.
func TestCLIProvidersBridgeDoesNotExecuteRawCallback(t *testing.T) {
	claude := NewClaudeBinProvider("sonnet", nil)
	cursor := NewCursorBinProvider("auto", nil)
	grok := NewGrokBinProvider("grok", nil)
	agy := NewAgyBinProvider("", nil)
	vibe := NewVibeBinProvider("", nil)
	cases := []struct {
		name   string
		setter toolExecutorSetter
		state  *cliToolBridgeState
	}{
		{"claude", claude, &claude.cliToolBridgeState},
		{"cursor", cursor, &cursor.cliToolBridgeState},
		{"grok", grok, &grok.cliToolBridgeState},
		{"agy", agy, &agy.cliToolBridgeState},
		{"vibe", vibe, &vibe.cliToolBridgeState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rawCalls atomic.Int32
			tc.setter.SetToolExecutor(func(context.Context, string, json.RawMessage) (core.ToolOutput, error) {
				rawCalls.Add(1)
				return core.TextOutput("raw callback must not run"), nil
			})
			bridge := &cliTurnBridge{toolReqCh: make(chan cliToolRequest, 1), done: make(chan struct{})}
			events := make(chan core.Event, 1)
			tc.state.activate(bridge, events)
			defer tc.state.deactivate(bridge)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := tc.state.wrappedExecutor(nil)(ctx, "spawn_script", json.RawMessage(`{"code":"text(1)"}`))
				result <- err
			}()
			var request cliToolRequest
			select {
			case request = <-bridge.toolReqCh:
			case <-ctx.Done():
				t.Fatal("bridge did not submit a request")
			}
			handleCLIToolRequest(request, eventSender{ctx: ctx, ch: events})
			event := <-events
			if event.Type != core.EventToolCall || event.Tool == nil || event.Tool.Name != "spawn_script" || event.ToolResponse == nil {
				t.Fatalf("bridge did not forward an engine-owned tool call: %+v", event)
			}
			event.ToolResponse <- core.ToolExecutionResponse{Result: core.TextOutput("engine result")}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if got := rawCalls.Load(); got != 0 {
				t.Fatalf("raw executor called %d times", got)
			}
		})
	}
}
