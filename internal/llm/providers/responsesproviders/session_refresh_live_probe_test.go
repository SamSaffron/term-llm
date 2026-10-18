package responsesproviders_test

// Opt-in, live compatibility experiment for session-input refresh planning.
// Sends only synthetic history and exposes only a constant-returning probe tool.
// Run each provider explicitly; ordinary go test skips this experiment.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	. "github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
)

func TestLiveSessionRefreshHistoricalTool(t *testing.T) {
	target := os.Getenv("TERM_LLM_SESSION_REFRESH_PROBE")
	if target == "" {
		t.Skip("set TERM_LLM_SESSION_REFRESH_PROBE to claude-bin or chatgpt")
	}
	model := "gpt-reserve"
	if target == "claude-bin" {
		model = "fable-medium"
	} else if target != "chatgpt" {
		t.Fatal("probe only supports the two explicitly requested providers")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	const name = "retired_probe_tool"
	const sentinel = "PROBE_RESULT_7319"
	spec := core.ToolSpec{Name: name, Description: "Synthetic compatibility probe; returns a fixed test marker and has no side effects.", Schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "required": []string{}, "additionalProperties": false}}
	other := spec
	other.Name = "unrelated_probe_tool"

	newProvider := func(t *testing.T) core.Provider {
		t.Helper()
		p, err := NewProviderByNameNoRetry(cfg, target, model)
		if err != nil {
			t.Fatal(err)
		}
		if chatgpt, ok := p.(*rp.ChatGPTProvider); ok {
			if transport := os.Getenv("TERM_LLM_SESSION_REFRESH_PROBE_TRANSPORT"); transport != "" {
				if transport != "http" && transport != "websocket" {
					t.Fatal("transport must be http or websocket")
				}
				rp.SetChatGPTUseWebSocketForTest(chatgpt, transport == "websocket")
			}
			t.Logf("ChatGPT configured WebSocket=%v", rp.ChatGPTUseWebSocketForTest(chatgpt))
		}
		if setter, ok := p.(engine.ToolExecutorSetter); ok {
			setter.SetToolExecutor(func(_ context.Context, tool string, _ json.RawMessage) (core.ToolOutput, error) {
				if tool != name && tool != other.Name {
					return core.ToolOutput{}, fmt.Errorf("probe refuses unexpected tool %q", tool)
				}
				return core.TextOutput(sentinel), nil
			})
		}
		if cleaner, ok := p.(interface{ CleanupMCP() }); ok {
			t.Cleanup(cleaner.CleanupMCP)
		}
		return p
	}

	workDir := t.TempDir()
	stream := func(t *testing.T, p core.Provider, req core.Request) (string, []core.ToolCall) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		req.Model = model
		req.WorkingDir = workDir
		req.MaxOutputTokens = 1024
		req.MaxTurns = 3
		s, err := p.Stream(ctx, req)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		defer s.Close()
		var text strings.Builder
		var calls []core.ToolCall
		for {
			e, err := s.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Recv: %v (text so far %q)", err, text.String())
			}
			switch e.Type {
			case core.EventTextDelta:
				text.WriteString(e.Text)
			case core.EventToolCall:
				if e.Tool != nil {
					calls = append(calls, *e.Tool)
				}
				// Inline CLI providers wait for the engine's acknowledgement.
				// This direct-provider probe supplies the same constant stub result.
				if e.ToolResponse != nil {
					if e.Tool == nil || (e.Tool.Name != name && e.Tool.Name != other.Name) {
						t.Fatal("unexpected inline tool request")
					}
					select {
					case e.ToolResponse <- core.ToolExecutionResponse{Result: core.TextOutput(sentinel)}:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			case core.EventToolExecStart:
				calls = append(calls, core.ToolCall{ID: e.ToolCallID, Name: e.ToolName, Arguments: e.ToolArgs})
			case core.EventError:
				t.Fatalf("provider error: %v (%s)", e.Err, e.Text)
			}
		}
		if chatgpt, ok := p.(*rp.ChatGPTProvider); ok {
			fallback, continued := chatgpt.WebSocketState()
			t.Logf("ChatGPT websocket_fallback=%v last_ws_request_has_previous_response=%v", fallback, continued)
			if rp.ChatGPTUseWebSocketForTest(chatgpt) && fallback {
				t.Fatal("WebSocket probe fell back to HTTP; transport result is inconclusive")
			}
		}
		t.Logf("provider=%s model=%s response=%q new_tool_calls=%d", target, model, text.String(), len(calls))
		return text.String(), calls
	}
	callMessage := func(c core.ToolCall) core.Message {
		return core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &c}}}
	}
	synthetic := []core.Message{
		core.SystemText("Answer the latest user request using the supplied history. Do not execute or repeat historical tool calls."),
		core.UserText("Retrieve the synthetic marker."),
		callMessage(core.ToolCall{ID: "call_retired_probe_7319", Name: name, Arguments: json.RawMessage(`{}`)}),
		core.ToolResultMessage("call_retired_probe_7319", name, sentinel, nil),
		core.AssistantText("The marker was retrieved."),
		core.UserText("What exact marker did the earlier tool return? Reply only with that marker. Do not call any tools."),
	}

	// Verify that the Responses adapter keeps this as a real, paired call/result;
	// otherwise successful replay would say nothing about unknown tool names.
	items := responses.BuildResponsesInput(synthetic)
	var wireCalls, wireResults int
	for _, item := range items {
		if item.Type == "function_call" && item.Name == name && item.CallID == "call_retired_probe_7319" {
			wireCalls++
		}
		if item.Type == "function_call_output" && item.CallID == "call_retired_probe_7319" {
			wireResults++
		}
	}
	if wireCalls != 1 || wireResults != 1 {
		t.Fatalf("probe history was not preserved structurally: calls=%d results=%d", wireCalls, wireResults)
	}

	for _, tc := range []struct {
		name  string
		tools []core.ToolSpec
	}{
		{"historical_tool_still_declared", []core.ToolSpec{spec}},
		{"historical_tool_removed_no_tools", nil},
		{"historical_tool_removed_other_tool_declared", []core.ToolSpec{other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t)
			text, calls := stream(t, p, core.Request{SessionID: fmt.Sprintf("refresh-probe-%d", time.Now().UnixNano()), Messages: synthetic, Tools: tc.tools})
			if !strings.Contains(text, sentinel) || len(calls) != 0 {
				t.Fatalf("expected historical marker without a new call; text=%q calls=%v", text, calls)
			}
		})
	}

	t.Run("same_provider_after_live_call_removal", func(t *testing.T) {
		p := newProvider(t)
		id := fmt.Sprintf("refresh-probe-native-%d", time.Now().UnixNano())
		history := []core.Message{core.SystemText("Follow the user's test instructions exactly. Only the synthetic probe tool is allowed."), core.UserText("Call retired_probe_tool exactly once with {} to retrieve its marker. Do not guess the result.")}
		_, calls := stream(t, p, core.Request{SessionID: id, Messages: history, Tools: []core.ToolSpec{spec}, ToolChoice: core.ToolChoice{Mode: core.ToolChoiceName, Name: name}})
		if len(calls) == 0 {
			t.Fatal("seed did not produce a real tool call; continuation probe inconclusive")
		}
		seen := map[string]bool{}
		for _, call := range calls {
			if seen[call.ID] {
				continue
			}
			seen[call.ID] = true
			if call.Name != name {
				t.Fatalf("unexpected seed call: %q", call.Name)
			}
			history = append(history, callMessage(call), core.ToolResultMessage(call.ID, call.Name, sentinel, nil))
		}
		history = append(history, core.UserText("The tool is now retired and absent from the available tools. What exact marker did it return? Reply only with that marker. Do not call any tools."))
		text, followupCalls := stream(t, p, core.Request{SessionID: id, Messages: history})
		if !strings.Contains(text, sentinel) || len(followupCalls) != 0 {
			t.Fatalf("expected marker after retirement; text=%q calls=%v", text, followupCalls)
		}
	})
}
