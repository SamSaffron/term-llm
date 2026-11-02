package clibin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestVibeCommandEnvForcesRestrictionsAndDropsAPIKey(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "inherited-key")
	t.Setenv("VIBE_ENABLED_TOOLS", `["bash"]`)
	t.Setenv("VIBE_ENABLE_SUBAGENTS", "true")
	t.Setenv("VIBE_HOME", "/custom/vibe-home")
	t.Setenv("VIBE_MCP_SERVERS", `[{"name":"external","command":"unsafe"}]`)
	home := t.TempDir()

	env := envMap(NewVibeBinProvider("glm-5-3", map[string]string{"EXTRA": "1", "VIBE_HOME": "/provider/vibe-home", "VIBE_MCP_SERVERS": "unsafe"}).buildVibeCommandEnv("glm-5-3", home))
	if _, ok := env["MISTRAL_API_KEY"]; ok {
		t.Fatal("inherited MISTRAL_API_KEY must not reach vibe-acp")
	}
	want := map[string]string{
		"VIBE_ENABLED_TOOLS":    `["termllm_*"]`,
		"VIBE_ENABLE_SUBAGENTS": "false",
		"VIBE_ACTIVE_MODEL":     "glm-5-3",
		"VIBE_HOME":             home,
		"VIBE_MCP_SERVERS":      "[]",
		"EXTRA":                 "1",
	}
	for key, value := range want {
		if env[key] != value {
			t.Errorf("%s = %q, want %q", key, env[key], value)
		}
	}

	if _, ok := envMap(NewVibeBinProvider("", nil).buildVibeCommandEnv("", t.TempDir()))["VIBE_ACTIVE_MODEL"]; ok {
		t.Fatal("empty model must keep Vibe's own default model")
	}
}

func TestVibeCommandEnvKeepsExplicitProviderAPIKey(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "inherited-key")
	env := envMap(NewVibeBinProvider("", map[string]string{"MISTRAL_API_KEY": "explicit"}).buildVibeCommandEnv("", t.TempDir()))
	if env["MISTRAL_API_KEY"] != "explicit" {
		t.Fatalf("MISTRAL_API_KEY = %q, want explicit provider env", env["MISTRAL_API_KEY"])
	}
}

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		out[key] = value
	}
	return out
}

func vibeUpdate(t *testing.T, update map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{"sessionId": "s", "update": update})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVibeACPHandlerApprovesOnlyTermLLMTools(t *testing.T) {
	h := newVibeACPHandler(false)
	for id, name := range map[string]string{"own": "mcp_termllm.read_file", "native": "bash", "other-mcp": "mcp_github.create_issue"} {
		h.HandleNotification(context.Background(), "session/update", vibeUpdate(t, map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": id, "_meta": map[string]any{"tool_name": name},
		}))
	}
	tests := map[string]string{"own": "selected", "native": "cancelled", "other-mcp": "cancelled", "unknown": "cancelled"}
	for id, want := range tests {
		params, _ := json.Marshal(map[string]any{"sessionId": "s", "toolCall": map[string]any{"toolCallId": id}})
		result, rpcErr := h.HandleRequest(context.Background(), "session/request_permission", params)
		if rpcErr != nil {
			t.Fatalf("%s: unexpected RPC error %v", id, rpcErr)
		}
		outcome := result.(map[string]any)["outcome"].(map[string]any)
		if outcome["outcome"] != want {
			t.Errorf("%s: outcome = %v, want %s", id, outcome, want)
		}
		if want == "selected" && outcome["optionId"] != "allow_once" {
			t.Errorf("%s: option = %v, want allow_once", id, outcome["optionId"])
		}
	}
	if _, rpcErr := h.HandleRequest(context.Background(), "fs/read_text_file", nil); rpcErr == nil {
		t.Fatal("client filesystem requests must be refused")
	}
}

func TestVibeACPHandlerMapsChunksAndSuppressesReplay(t *testing.T) {
	h := newVibeACPHandler(false)
	chunk := func(kind, messageID, text string) json.RawMessage {
		return vibeUpdate(t, map[string]any{"sessionUpdate": kind, "messageId": messageID, "content": map[string]any{"type": "text", "text": text}})
	}
	// History replayed by session/load arrives with no active turn.
	h.HandleNotification(context.Background(), "session/update", chunk("agent_message_chunk", "old", "replayed"))

	ch := make(chan core.Event, 8)
	h.beginTurn(eventSender{ctx: context.Background(), ch: ch})
	h.HandleNotification(context.Background(), "session/update", chunk("agent_thought_chunk", "reasoning-1", "thinking"))
	h.HandleNotification(context.Background(), "session/update", chunk("agent_message_chunk", "assistant-1", "answer"))
	h.HandleNotification(context.Background(), "session/update", chunk("user_message_chunk", "user-1", "echo"))
	h.endTurn()
	close(ch)

	var got []core.Event
	for event := range ch {
		got = append(got, event)
	}
	if len(got) != 2 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Type != core.EventReasoningDelta || got[0].Text != "thinking" || got[0].ReasoningItemID != "vibe-reasoning-1" {
		t.Fatalf("reasoning event = %+v", got[0])
	}
	if got[1].Type != core.EventTextDelta || got[1].Text != "answer" {
		t.Fatalf("text event = %+v", got[1])
	}
}

func TestVibeACPHandlerTracksUsageUpdates(t *testing.T) {
	h := newVibeACPHandler(false)
	h.HandleNotification(context.Background(), "session/update", vibeUpdate(t, map[string]any{
		"sessionUpdate": "usage_update",
		"_meta":         map[string]any{"promptTokens": 120, "completionTokens": 30, "cachedTokens": 50},
	}))
	baseline := h.beginTurn(eventSender{ctx: context.Background(), ch: make(chan core.Event, 1)})
	h.endTurn()
	if baseline != (vibeUsageCounters{input: 120, output: 30, cached: 50}) {
		t.Fatalf("baseline = %+v", baseline)
	}
}

func TestVibeTurnUsage(t *testing.T) {
	tests := []struct {
		name            string
		baseline, final vibeUsageCounters
		want            *core.Usage
	}{
		{
			name:     "delta from cumulative session counters",
			baseline: vibeUsageCounters{input: 1000, output: 100, cached: 400},
			final:    vibeUsageCounters{input: 1500, output: 160, cached: 700},
			want:     &core.Usage{InputTokens: 200, OutputTokens: 60, CachedInputTokens: 300, ProviderRawInputTokens: 500, ProviderTotalTokens: 560},
		},
		{
			name:     "counter reset uses the final values",
			baseline: vibeUsageCounters{input: 1000, output: 100},
			final:    vibeUsageCounters{input: 80, output: 20, cached: 30},
			want:     &core.Usage{InputTokens: 50, OutputTokens: 20, CachedInputTokens: 30, ProviderRawInputTokens: 80, ProviderTotalTokens: 100},
		},
		{
			name:     "no new tokens",
			baseline: vibeUsageCounters{input: 10, output: 5},
			final:    vibeUsageCounters{input: 10, output: 5},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := vibeTurnUsage(tt.baseline, tt.final)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("usage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseVibeUsageRejectsInvalidCounts(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `{"inputTokens":5}`, `{"inputTokens":5,"outputTokens":-1}`, `{"inputTokens":5,"outputTokens":1,"cachedReadTokens":9}`} {
		if _, ok := parseVibeUsage(json.RawMessage(raw)); ok {
			t.Errorf("parseVibeUsage(%s) accepted invalid usage", raw)
		}
	}
	got, ok := parseVibeUsage(json.RawMessage(`{"totalTokens":34645,"inputTokens":34351,"outputTokens":294,"cachedReadTokens":20736}`))
	if !ok || got != (vibeUsageCounters{input: 34351, output: 294, cached: 20736}) {
		t.Fatalf("parseVibeUsage = %+v, %t", got, ok)
	}
}

func TestCheckVibeModel(t *testing.T) {
	options := json.RawMessage(`[{"id":"mode","currentValue":"accept-edits"},{"id":"model","currentValue":"mistral-large-4-0-3","options":[{"value":"mistral-large-4-0-3"},{"value":"glm-5-3"}]}]`)
	if err := checkVibeModel(options, "mistral-large-4-0-3"); err != nil {
		t.Fatalf("matching model: %v", err)
	}
	if err := checkVibeModel(options, ""); err != nil {
		t.Fatalf("default model: %v", err)
	}
	err := checkVibeModel(options, "mistral-tiny")
	var userErr *UserFacingProviderError
	if !errors.As(err, &userErr) || !strings.Contains(userErr.Detail, "mistral-large-4-0-3, glm-5-3") {
		t.Fatalf("unknown model error = %v", err)
	}
}

func TestVibeProviderStateRoundTripAndValidation(t *testing.T) {
	p := NewVibeBinProvider("", nil)
	if _, ok := p.ExportProviderState(); ok {
		t.Fatal("no session should export no state")
	}
	p.sessionID, p.messagesSent = "session-1", 4
	data, ok := p.ExportProviderState()
	if !ok {
		t.Fatal("expected exported state")
	}
	restored := NewVibeBinProvider("", nil)
	if err := restored.ImportProviderState(data); err != nil {
		t.Fatal(err)
	}
	if restored.sessionID != "session-1" || restored.messagesSent != 4 {
		t.Fatalf("restored = %q/%d", restored.sessionID, restored.messagesSent)
	}
	for _, bad := range []string{`{`, `{"session_id":" "}`, `{"session_id":"s","messages_sent":-1}`} {
		if err := NewVibeBinProvider("", nil).ImportProviderState([]byte(bad)); err == nil {
			t.Errorf("ImportProviderState(%s) accepted invalid state", bad)
		}
	}
}

func TestVibeMessagesForRequestSendsOnlyNewUserTurn(t *testing.T) {
	p := NewVibeBinProvider("", nil)
	p.sessionID, p.messagesSent = "s", 3
	messages := []core.Message{
		core.SystemText("system"), core.UserText("first"), core.AssistantText("reply"),
		core.AssistantText("late assistant output"), core.UserText("second"),
	}
	got, err := p.messagesForRequest(core.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || core.CollectTextParts(got[0].Parts) != "second" {
		t.Fatalf("messages = %+v", got)
	}
	if _, err := p.messagesForRequest(core.Request{Messages: messages[:3]}); err == nil {
		t.Fatal("expected an error when no new messages exist")
	}
}

func TestVibePromptBlocksPrefixesSystemInstructions(t *testing.T) {
	policy := vibeToolPolicy([]core.ToolSpec{{Name: "read_file"}})
	blocks, err := vibePromptBlocks([]core.Message{core.SystemText("be terse"), core.UserText("hello")}, policy, "be terse")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || !strings.HasPrefix(blocks[0].Text, "<system_instructions>\n"+policy+"\n\nbe terse") || blocks[1].Text != "hello" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if !strings.Contains(policy, "`read_file`") || !strings.Contains(policy, "`mcp_termllm.<name>`") || !strings.Contains(policy, `functions ["mcp_termllm.read_file"]`) {
		t.Fatalf("tool policy must name term-llm tools and their prefix: %q", policy)
	}
	if strings.Contains(vibeToolPolicy(nil), "mcp_termllm") {
		t.Fatal("tool-free policy must not reference term-llm tools")
	}
}

// fakeVibeACP is a minimal vibe-acp stand-in. It logs its environment and
// every request, reports cumulative usage, and echoes the prompt text count.
const fakeVibeACP = `#!/bin/sh
{
  echo "ENV MISTRAL_API_KEY=${MISTRAL_API_KEY-unset}"
  echo "ENV VIBE_ENABLED_TOOLS=$VIBE_ENABLED_TOOLS"
  echo "ENV VIBE_ACTIVE_MODEL=$VIBE_ACTIVE_MODEL"
  echo "PWD $(pwd)"
} >> "$VIBE_FAKE_LOG"
turn=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$VIBE_FAKE_LOG"
  id=${line#*\"id\":}
  id=${id%%,*}
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{\"loadSession\":true},\"authMethods\":[]}}" ;;
    *'"method":"session/new"'*)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"sessionId\":\"vibe-session\",\"configOptions\":[{\"id\":\"model\",\"currentValue\":\"glm-5-3\",\"options\":[{\"value\":\"glm-5-3\"}]}]}}" ;;
    *'"method":"session/load"'*)
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"vibe-session","update":{"sessionUpdate":"agent_message_chunk","messageId":"old","content":{"type":"text","text":"REPLAYED"}}}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"vibe-session","update":{"sessionUpdate":"usage_update","_meta":{"promptTokens":500,"completionTokens":50,"cachedTokens":0}}}}'
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}" ;;
    *'"method":"session/set_config_option"'*)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"configOptions\":[]}}" ;;
    *'"method":"session/prompt"'*)
      turn=$((turn + 1))
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"vibe-session","update":{"sessionUpdate":"agent_thought_chunk","messageId":"reasoning-1","content":{"type":"text","text":"pondering"}}}}'
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"vibe-session\",\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"messageId\":\"assistant-$turn\",\"content\":{\"type\":\"text\",\"text\":\"reply $turn\"}}}}"
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"stopReason\":\"end_turn\",\"usage\":{\"inputTokens\":$((500 + turn * 100)),\"outputTokens\":$((50 + turn * 10)),\"cachedReadTokens\":$((turn * 40))}}}" ;;
  esac
done
`

func installFakeVibeACP(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, vibeACPBinary), []byte(fakeVibeACP), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	logPath := filepath.Join(t.TempDir(), "vibe.log")
	t.Setenv("VIBE_FAKE_LOG", logPath)
	t.Setenv("MISTRAL_API_KEY", "must-not-leak")
	return logPath
}

func collectVibeTestEvents(t *testing.T, p *VibeBinProvider, req core.Request) []core.Event {
	t.Helper()
	stream, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var events []core.Event
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == core.EventError {
			t.Fatal(event.Err)
		}
		events = append(events, event)
	}
}

func vibeEventText(events []core.Event, kind core.EventType) string {
	var text strings.Builder
	for _, event := range events {
		if event.Type == kind {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

func vibeEventUsage(events []core.Event) *core.Usage {
	for _, event := range events {
		if event.Type == core.EventUsage {
			return event.Use
		}
	}
	return nil
}

func TestVibeBinProviderStreamsAndReusesSession(t *testing.T) {
	logPath := installFakeVibeACP(t)
	p := NewVibeBinProvider("glm-5-3", nil)
	defer p.CleanupMCP()

	messages := []core.Message{core.SystemText("SYSTEM-RULES"), core.UserText("first question")}
	events := collectVibeTestEvents(t, p, core.Request{Messages: messages})
	if got := vibeEventText(events, core.EventTextDelta); got != "reply 1" {
		t.Fatalf("text = %q", got)
	}
	if got := vibeEventText(events, core.EventReasoningDelta); got != "pondering" {
		t.Fatalf("reasoning = %q", got)
	}
	if usage := vibeEventUsage(events); usage == nil || usage.ProviderRawInputTokens != 600 || usage.CachedInputTokens != 40 || usage.OutputTokens != 60 {
		t.Fatalf("first usage = %+v", usage)
	}
	if p.sessionID != "vibe-session" || p.messagesSent != 2 {
		t.Fatalf("state = %q/%d", p.sessionID, p.messagesSent)
	}

	messages = append(messages, core.AssistantText("reply 1"), core.UserText("second question"))
	events = collectVibeTestEvents(t, p, core.Request{Messages: messages})
	if usage := vibeEventUsage(events); usage == nil || usage.ProviderRawInputTokens != 100 || usage.OutputTokens != 10 {
		t.Fatalf("second turn usage must be a delta, got %+v", usage)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, want := range []string{"ENV MISTRAL_API_KEY=unset", `ENV VIBE_ENABLED_TOOLS=["termllm_*"]`, "ENV VIBE_ACTIVE_MODEL=glm-5-3", filepath.Join("term-llm", "vibe-bin", "cwd")} {
		if !strings.Contains(log, want) {
			t.Errorf("fake vibe-acp log missing %q:\n%s", want, log)
		}
	}
	if strings.Count(log, `"method":"session/new"`) != 1 || strings.Count(log, "ENV VIBE_ACTIVE_MODEL") != 1 {
		t.Fatalf("expected one process and session for both turns:\n%s", log)
	}
	prompts := strings.Split(log, `"method":"session/prompt"`)
	if len(prompts) != 3 || !strings.Contains(prompts[1], "SYSTEM-RULES") || strings.Contains(prompts[2], "SYSTEM-RULES") || strings.Contains(prompts[2], "first question") || !strings.Contains(prompts[2], "second question") {
		t.Fatalf("unexpected prompts:\n%s", log)
	}
	// Only the session-scoped max_turns may be set: Vibe persists the model
	// and thinking options as the user's defaults.
	if strings.Count(log, `"method":"session/set_config_option"`) != 1 || !strings.Contains(log, `"configId":"max_turns","sessionId":"vibe-session","value":"30"`) {
		t.Fatalf("expected only the session max_turns option:\n%s", log)
	}
}

func TestVibeBinProviderResumesImportedSession(t *testing.T) {
	logPath := installFakeVibeACP(t)
	p := NewVibeBinProvider("", nil)
	defer p.CleanupMCP()
	if err := p.ImportProviderState([]byte(`{"session_id":"vibe-session","messages_sent":2}`)); err != nil {
		t.Fatal(err)
	}
	events := collectVibeTestEvents(t, p, core.Request{Messages: []core.Message{
		core.UserText("old question"), core.AssistantText("old reply"), core.UserText("follow up"),
	}})
	if got := vibeEventText(events, core.EventTextDelta); got != "reply 1" {
		t.Fatalf("text = %q (replayed history must not be re-emitted)", got)
	}
	if usage := vibeEventUsage(events); usage == nil || usage.ProviderRawInputTokens != 100 {
		t.Fatalf("usage after load must use the replayed baseline, got %+v", usage)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	if !strings.Contains(log, `"method":"session/load"`) || strings.Contains(log, `"method":"session/new"`) {
		t.Fatalf("expected session/load only:\n%s", log)
	}
	if prompt := log[strings.Index(log, `"method":"session/prompt"`):]; strings.Contains(prompt, "old question") || !strings.Contains(prompt, "follow up") {
		t.Fatalf("resumed prompt must carry only the new turn:\n%s", prompt)
	}
}

func TestVibeBinProviderReportsMissingBinary(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	p := NewVibeBinProvider("", nil)
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		var userErr *UserFacingProviderError
		if errors.As(err, &userErr) || (event.Type == core.EventError && errors.As(event.Err, &userErr)) {
			if userErr.Summary != "Mistral Vibe CLI is not installed" {
				t.Fatalf("summary = %q", userErr.Summary)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
	}
}

// The credentialed smoke tests run the real vibe-acp with the caller's Vibe
// sign-in. Run them outside scripts/gotest.sh, which isolates HOME.
func TestVibeBinProviderRealSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_VIBE_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_VIBE_ACP=1 to run the credentialed Vibe ACP smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewVibeBinProvider("zai-glm-5-3-flash", nil)
	defer p.CleanupMCP()
	events := collectVibeTestEvents(t, p, core.Request{Messages: []core.Message{
		core.SystemText("Reply concisely and do not use tools."),
		core.UserText("Reply with exactly: REAL VIBE OK"),
	}})
	if text := vibeEventText(events, core.EventTextDelta); !strings.Contains(text, "REAL VIBE OK") {
		t.Fatalf("text = %q", text)
	}
	if usage := vibeEventUsage(events); usage == nil || usage.ProviderRawInputTokens <= 0 || usage.OutputTokens <= 0 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestVibeBinProviderRealToolSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_VIBE_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_VIBE_ACP=1 to run the credentialed Vibe ACP tool smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewVibeBinProvider("zai-glm-5-3-flash", nil)
	p.SetToolExecutor(func(context.Context, string, json.RawMessage) (core.ToolOutput, error) {
		return core.TextOutput("ECHO_TOOL_OK"), nil
	})
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), core.Request{
		Messages: []core.Message{
			core.SystemText("Use only the supplied term-llm tool when asked."),
			core.UserText("Call the echo_once tool exactly once, then reply with its result."),
		},
		Tools: []core.ToolSpec{{
			Name:        "echo_once",
			Description: "Returns the exact text ECHO_TOOL_OK. Use when explicitly asked.",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	toolCalls := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case core.EventError:
			t.Fatal(event.Err)
		case core.EventToolCall:
			toolCalls++
			if event.ToolName != "echo_once" || event.ToolResponse == nil {
				t.Fatalf("tool event = %+v", event)
			}
			event.ToolResponse <- core.ToolExecutionResponse{Result: core.TextOutput("ECHO_TOOL_OK")}
		case core.EventTextDelta:
			text += event.Text
		}
	}
	if toolCalls != 1 || !strings.Contains(text, "ECHO_TOOL_OK") {
		t.Fatalf("tool calls=%d text=%q", toolCalls, text)
	}
}

// A term-llm tool that shares a name with a Vibe built-in must still be the one
// called; the built-in is always denied.
func TestVibeBinProviderRealCollidingToolSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_VIBE_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_VIBE_ACP=1 to run the credentialed Vibe ACP tool smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewVibeBinProvider("zai-glm-5-3-flash", nil)
	p.SetToolExecutor(func(context.Context, string, json.RawMessage) (core.ToolOutput, error) {
		return core.TextOutput("unused"), nil
	})
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("Use the read_file tool to read /project/notes.txt and reply with its exact contents.")},
		Tools: []core.ToolSpec{{
			Name:        "read_file",
			Description: "Read a text file.",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	toolCalls := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case core.EventError:
			t.Fatal(event.Err)
		case core.EventToolCall:
			toolCalls++
			event.ToolResponse <- core.ToolExecutionResponse{Result: core.TextOutput("NOTES_CONTENT_42")}
		case core.EventTextDelta:
			text += event.Text
		}
	}
	if toolCalls == 0 || !strings.Contains(text, "NOTES_CONTENT_42") {
		t.Fatalf("tool calls=%d text=%q", toolCalls, text)
	}
}
