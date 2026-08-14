package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/acp"
)

type observedWriteCloser struct {
	io.WriteCloser
	started chan struct{}
	once    sync.Once
}

func (w *observedWriteCloser) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.WriteCloser.Write(data)
}

func TestStopGrokACPProcessUnblocksBlockedWrite(t *testing.T) {
	oldCloseTimeout := grokACPCloseTimeout
	grokACPCloseTimeout = 100 * time.Millisecond
	t.Cleanup(func() { grokACPCloseTimeout = oldCloseTimeout })

	clientSide, agentSide := net.Pipe()
	defer clientSide.Close()
	defer agentSide.Close()

	stdin := &observedWriteCloser{WriteCloser: clientSide, started: make(chan struct{})}
	conn := acp.NewConnection(clientSide, stdin, nil, acp.Options{})
	waitDone := make(chan struct{})
	process := &grokACPProcess{
		cancel:   func() { close(waitDone) },
		stdin:    stdin,
		client:   acp.NewClient(conn),
		conn:     conn,
		waitDone: waitDone,
		capabilities: acp.AgentCapabilities{SessionCapabilities: acp.SessionCapabilities{
			Close: json.RawMessage(`true`),
		}},
		sessionID: "blocked-session",
	}

	callDone := make(chan error, 1)
	go func() {
		callDone <- conn.Call(context.Background(), "session/prompt", map[string]string{
			"prompt": strings.Repeat("x", 1<<20),
		}, nil)
	}()
	select {
	case <-stdin.started:
	case <-time.After(time.Second):
		t.Fatal("ACP call did not start its blocked write")
	}

	stopDone := make(chan struct{})
	go func() {
		var provider GrokBinProvider
		provider.stopGrokACPProcess(process)
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Grok ACP process stop remained blocked behind the stdin write")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("blocked ACP call unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked ACP call did not exit after stdin was closed")
	}
}

func TestParseGrokACPUsage(t *testing.T) {
	usage, err := parseGrokACPUsage(json.RawMessage(`{
		"inputTokens":7333,
		"outputTokens":38,
		"cachedReadTokens":7296,
		"reasoningTokens":30,
		"totalTokens":7372
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.InputTokens != 37 || usage.CachedInputTokens != 7296 || usage.OutputTokens != 38 || usage.ReasoningTokens != 30 || usage.ProviderRawInputTokens != 7333 || usage.ProviderTotalTokens != 7372 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestParseGrokACPUsageRejectsMissingOrInvalidCounts(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"inputTokens":-1,"outputTokens":2}`,
		`{"inputTokens":1,"outputTokens":2,"cachedReadTokens":3}`,
		`{"inputTokens":"many","outputTokens":2}`,
	} {
		usage, err := parseGrokACPUsage(json.RawMessage(raw))
		if err == nil && usage != nil {
			t.Fatalf("parseGrokACPUsage(%s) = %+v, want absent/error", raw, usage)
		}
	}
}

func TestGrokACPHandlerMapsTextAndReasoningButNotToolExecution(t *testing.T) {
	events := make(chan Event, 4)
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{ctx: context.Background(), ch: events}, false, false)
	defer handler.endTurn()

	for _, params := range []string{
		`{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}`,
		`{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"answer"}}}`,
	} {
		handler.HandleNotification(context.Background(), "session/update", json.RawMessage(params))
	}

	if err := handler.turnError(); err != nil {
		t.Fatal(err)
	}
	close(events)
	var got []Event
	for event := range events {
		got = append(got, event)
	}
	if len(got) != 2 || got[0].Type != EventReasoningDelta || got[0].Text != "thinking" || got[1].Type != EventTextDelta || got[1].Text != "answer" {
		t.Fatalf("events = %+v", got)
	}
}

func TestGrokACPHandlerSeparatesThoughtsAcrossHiddenToolCall(t *testing.T) {
	events := make(chan Event, 2)
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{ctx: context.Background(), ch: events}, false, false)
	defer handler.endTurn()

	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"first."}}}`))
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"search-1","_meta":{"x.ai/tool":{"name":"search_tool"}}}}`))
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"Good"}}}`))

	first, second := <-events, <-events
	if first.ReasoningItemID == "" || second.ReasoningItemID == "" || first.ReasoningItemID == second.ReasoningItemID {
		t.Fatalf("reasoning item IDs across hidden tool call = %q, %q; want distinct non-empty IDs", first.ReasoningItemID, second.ReasoningItemID)
	}
}

func TestGrokACPHandlerKeepsPolicyViolationAfterCancelledDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{ctx: ctx, ch: make(chan Event)}, false, false)
	defer handler.endTurn()

	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"late"}}}`))
	if err := handler.turnError(); !errors.Is(err, context.Canceled) {
		t.Fatalf("delivery error = %v, want cancellation", err)
	}
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"bad","_meta":{"x.ai/tool":{"name":"read_file"}}}}`))
	if err := handler.policyError(); err == nil || !strings.Contains(err.Error(), "restricted profile attempted native tool") {
		t.Fatalf("policy error = %v", err)
	}
}

func TestGrokACPHandlerRejectsNativeToolLeak(t *testing.T) {
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{}, false, false)
	defer handler.endTurn()
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"native-1","title":"Read file","_meta":{"x.ai/tool":{"name":"read_file"}}}}`))
	if err := handler.turnError(); err == nil || !strings.Contains(err.Error(), "native tool") {
		t.Fatalf("native tool error = %v", err)
	}
}

func TestGrokACPHandlerRejectsNativeToolLeakWithRichContent(t *testing.T) {
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{}, false, false)
	defer handler.endTurn()
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"native-1","content":[{"type":"content","content":{"type":"text","text":"details"}}],"_meta":{"x.ai/tool":{"name":"read_file"}}}}`))
	if err := handler.turnError(); err == nil || !strings.Contains(err.Error(), "native tool") {
		t.Fatalf("native tool error = %v", err)
	}
}

func TestGrokACPHandlerCancelsPermissionRequests(t *testing.T) {
	handler := &grokACPHandler{}
	result, rpcErr := handler.HandleRequest(context.Background(), "session/request_permission", json.RawMessage(`{"sessionId":"s","options":[]}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"outcome":{"outcome":"cancelled"}}` {
		t.Fatalf("permission response = %s", encoded)
	}
}

func TestGrokACPToolBarrierPreservesTextOrder(t *testing.T) {
	events := make(chan Event, 4)
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{ctx: context.Background(), ch: events}, false, false)
	defer handler.endTurn()

	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"before tool"}}}`))
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"use-1","title":"Calling echo_once","_meta":{"x.ai/tool":{"name":"use_tool"}}}}`))
	if err := handler.waitToolBarrier(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	toolRequest := cliToolRequest{
		callID:   "mcp-test-1",
		name:     "test_tool",
		args:     json.RawMessage(`{}`),
		response: make(chan ToolExecutionResponse, 1),
		ack:      make(chan error, 1),
	}
	handleCLIToolRequest(toolRequest, eventSender{ctx: context.Background(), ch: events})
	if err := <-toolRequest.ack; err != nil {
		t.Fatal(err)
	}
	first, second := <-events, <-events
	if first.Type != EventTextDelta || first.Text != "before tool" || second.Type != EventToolCall {
		t.Fatalf("ordered events = %+v then %+v", first, second)
	}
}

func TestGrokACPHandlerSuppressesLoadReplay(t *testing.T) {
	events := make(chan Event, 1)
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{ctx: context.Background(), ch: events}, true, false)
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"old"}}}`))
	handler.endTurn()
	select {
	case event := <-events:
		t.Fatalf("replayed event leaked: %+v", event)
	default:
	}
}

func TestGrokBinProviderBuildACPArgsUsesRestrictedProfile(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5-high", nil)
	p.grokHome = t.TempDir()
	profilePath, err := p.writeACPAgentProfile(false)
	if err != nil {
		t.Fatal(err)
	}
	args, effort, err := p.buildACPArgs(Request{}, profilePath)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--no-auto-update",
		"--max-turns 30",
		"agent",
		"--agent-profile " + profilePath,
		"--no-leader",
		"-m grok-4.5",
		"--reasoning-effort high",
		"--always-approve",
		"stdio",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if effort != "high" {
		t.Fatalf("effort = %q", effort)
	}
	disableWebSearch := slices.Index(args, "--disable-web-search")
	agentCommand := slices.Index(args, "agent")
	if disableWebSearch < 0 || agentCommand < 0 || disableWebSearch > agentCommand {
		t.Fatalf("root --disable-web-search flag must precede agent subcommand: %q", args)
	}
	disallowed := "," + argValue(args, "--disallowed-tools") + ","
	for _, tool := range []string{"web_search", "web_fetch", "x_search"} {
		if !strings.Contains(disallowed, ","+tool+",") {
			t.Fatalf("restricted ACP args did not disallow %q: %s", tool, disallowed)
		}
	}
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(profile)
	for _, want := range []string{
		"promptMode: full",
		"permissionMode: default",
		"agentsMd: false",
		"discoverSkills: false",
		"inheritSkills: false",
		"skills: []",
		"tools:\n  - search_tool\n  - use_tool",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("restricted ACP profile missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"prompt_mode:", "permission_mode:", "agents_md:", "  - web_search", "  - web_fetch", "  - x_search", "list_dir"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unsafe agent profile contains %q:\n%s", forbidden, text)
		}
	}
}

func TestGrokACPPromptMetaUsesVerbatimModeAndPromptID(t *testing.T) {
	meta, promptID, err := grokACPPromptMeta()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Verbatim bool   `json:"verbatim"`
		PromptID string `json:"promptId"`
	}
	if err := json.Unmarshal(meta, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Verbatim || decoded.PromptID == "" || decoded.PromptID != promptID || !isGrokHomeID(promptID) {
		t.Fatalf("prompt metadata = %s, promptID=%q", meta, promptID)
	}
}

func TestGrokACPWirePromptUsesVerbatimMetadata(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	promptLog := filepath.Join(t.TempDir(), "prompt.json")
	binDir := t.TempDir()
	script := `#!/bin/sh
while IFS= read -r line; do
  id=${line#*\"id\":}
  id=${id%%,*}
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{},\"authMethods\":[{\"id\":\"cached_token\",\"name\":\"Cached\"}]}}" ;;
    *'"method":"authenticate"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}" ;;
    *'"method":"session/new"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"sessionId\":\"wire-session\"}}" ;;
    *'"method":"session/prompt"'*)
      printf '%s' "$line" > "$GROK_PROMPT_LOG"
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"stopReason\":\"end_turn\"}}"
      ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(binDir, "grok"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := NewGrokBinProvider("grok-4.6-low", map[string]string{"GROK_PROMPT_LOG": promptLog})
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{SystemText("system"), UserText(strings.Repeat("x", 30_000))}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	raw, err := os.ReadFile(promptLog)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Params struct {
			Meta struct {
				Verbatim bool   `json:"verbatim"`
				PromptID string `json:"promptId"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatalf("decode prompt request: %v: %s", err, raw)
	}
	if !request.Params.Meta.Verbatim || !isGrokHomeID(request.Params.Meta.PromptID) {
		t.Fatalf("wire prompt metadata = %+v", request.Params.Meta)
	}
}

func TestGrokACPAgentCancelledTurnEmitsWarningAndCommits(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	script := `#!/bin/sh
while IFS= read -r line; do
  id=${line#*\"id\":}
  id=${id%%,*}
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{},\"authMethods\":[{\"id\":\"cached_token\",\"name\":\"Cached\"}]}}" ;;
    *'"method":"authenticate"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}" ;;
    *'"method":"session/new"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"sessionId\":\"agent-cancel-session\"}}" ;;
    *'"method":"session/prompt"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"stopReason\":\"cancelled\"}}" ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(binDir, "grok"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := NewGrokBinProvider("grok-4.6-low", nil)
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{UserText("cancel this")}})
	if err != nil {
		t.Fatal(err)
	}
	var warning string
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		if event.Type == EventPhase {
			warning += event.Text
		}
	}
	if !strings.Contains(warning, "cancelled the turn") || p.sessionID != "agent-cancel-session" || p.messagesSent != 1 {
		t.Fatalf("warning/state = %q %q/%d", warning, p.sessionID, p.messagesSent)
	}
}

func TestGrokBinProviderBuildACPArgsEnablesNativeWebAndXSearch(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5-low", nil)
	p.grokHome = t.TempDir()
	profilePath, err := p.writeACPAgentProfile(true)
	if err != nil {
		t.Fatal(err)
	}
	args, _, err := p.buildACPArgs(Request{Search: true}, profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := argValue(args, "--tools"); got != grokNativeSearchToolAllowlist {
		t.Fatalf("--tools = %q", got)
	}
	if slices.Contains(args, "--disable-web-search") {
		t.Fatalf("native search args unexpectedly disable web search: %q", args)
	}
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  - web_search", "  - web_fetch", "  - x_search", "read-only web and X research"} {
		if !strings.Contains(string(profile), want) {
			t.Fatalf("native search profile missing %q:\n%s", want, profile)
		}
	}
}

func TestGrokACPHandlerReportsNativeSearchCallDetails(t *testing.T) {
	events := make(chan Event, 2)
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{ctx: context.Background(), ch: events}, false, true)
	defer handler.endTurn()

	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"x-1","title":"X search:","kind":"search","status":"in_progress","rawInput":{"variant":"XSearch","backend":true},"_meta":{"backend":true}}}`))
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"x-1","title":"X search:","status":"completed","rawOutput":{"input":"{\"query\":\"from:SpaceXAI Grok\",\"limit\":\"10\",\"mode\":\"Latest\"}","name":"x_keyword_search"}}}`))

	if err := handler.turnError(); err != nil {
		t.Fatal(err)
	}
	start, end := <-events, <-events
	wantInfo := "(limit:10, mode:Latest, query:from:SpaceXAI Grok)"
	if start.Type != EventToolExecStart || start.ToolCallID != "x-1" || start.ToolName != "x_keyword_search" || start.ToolInfo != wantInfo {
		t.Fatalf("start event = %+v", start)
	}
	if string(start.ToolArgs) != `{"query":"from:SpaceXAI Grok","limit":"10","mode":"Latest"}` {
		t.Fatalf("start args = %s", start.ToolArgs)
	}
	if end.Type != EventToolExecEnd || end.ToolCallID != "x-1" || end.ToolName != "x_keyword_search" || end.ToolInfo != wantInfo || !end.ToolSuccess {
		t.Fatalf("end event = %+v", end)
	}
}

func TestGrokACPHandlerReportsNativeSearchWithoutBackendArguments(t *testing.T) {
	events := make(chan Event, 2)
	handler := &grokACPHandler{}
	handler.beginTurn(eventSender{ctx: context.Background(), ch: events}, false, true)
	defer handler.endTurn()

	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"web-1","title":"Web search:","kind":"search","status":"in_progress","rawInput":{"variant":"WebSearch","backend":true},"_meta":{"backend":true}}}`))
	handler.HandleNotification(context.Background(), "session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"web-1","title":"Web search:","status":"failed","rawOutput":{"action":"search","status":"failed"}}}`))

	start, end := <-events, <-events
	if start.Type != EventToolExecStart || start.ToolName != "web_search" || len(start.ToolArgs) != 0 {
		t.Fatalf("start event = %+v", start)
	}
	if end.Type != EventToolExecEnd || end.ToolName != "web_search" || end.ToolSuccess {
		t.Fatalf("end event = %+v", end)
	}
}

func TestGrokACPHandlerAllowsNativeSearchOnlyWhenEnabled(t *testing.T) {
	for _, tc := range []struct {
		name          string
		nativeSearch  bool
		update        string
		wantTurnError bool
	}{
		{name: "named disabled", update: `{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"web-1","_meta":{"x.ai/tool":{"name":"web_search"}}}}`, wantTurnError: true},
		{name: "named enabled", nativeSearch: true, update: `{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"web-1","_meta":{"x.ai/tool":{"name":"web_search"}}}}`},
		{name: "backend disabled", update: `{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"x-1","kind":"search","_meta":{"backend":true}}}`, wantTurnError: true},
		{name: "backend enabled", nativeSearch: true, update: `{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"x-1","kind":"search","_meta":{"backend":true}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &grokACPHandler{}
			handler.beginTurn(eventSender{}, false, tc.nativeSearch)
			defer handler.endTurn()
			handler.HandleNotification(context.Background(), "session/update", json.RawMessage(tc.update))
			if got := handler.turnError() != nil; got != tc.wantTurnError {
				t.Fatalf("turn error = %v, want error=%t", handler.turnError(), tc.wantTurnError)
			}
		})
	}
}

func TestGrokBinProviderACPStreamEmitsUsage(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.5-low", nil)
	p.acpRunner = func(_ context.Context, _ Request, _ []Message, _ bool, send eventSender, _ bool) (grokCommandResult, error) {
		if err := os.WriteFile(filepath.Join(p.grokHome, "fake-args"), []byte("agent stdio"), 0o644); err != nil {
			return grokCommandResult{}, err
		}
		for _, event := range []Event{
			{Type: EventReasoningDelta, Text: "thinking"},
			{Type: EventTextDelta, Text: "answer"},
			{Type: EventUsage, Use: &Usage{InputTokens: 8, CachedInputTokens: 92, OutputTokens: 20, ReasoningTokens: 7, ProviderRawInputTokens: 100, ProviderTotalTokens: 121}},
		} {
			if err := send.Send(event); err != nil {
				return grokCommandResult{}, err
			}
		}
		return grokCommandResult{sawEnd: true, sessionID: "fake-session"}, nil
	}
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{SystemText("private system"), UserText("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		got = append(got, event)
	}
	if len(got) != 4 || got[0].Type != EventReasoningDelta || got[1].Type != EventTextDelta || got[2].Type != EventUsage || got[3].Type != EventDone {
		t.Fatalf("events = %+v", got)
	}
	if got[2].Use == nil || got[2].Use.InputTokens != 8 || got[2].Use.CachedInputTokens != 92 || got[2].Use.OutputTokens != 20 || got[2].Use.ReasoningTokens != 7 || got[2].Use.ProviderTotalTokens != 121 {
		t.Fatalf("usage event = %+v", got[2])
	}
	if p.sessionID != "fake-session" || p.messagesSent != 2 {
		t.Fatalf("provider state = %q/%d", p.sessionID, p.messagesSent)
	}

	args, err := os.ReadFile(filepath.Join(p.grokHome, "fake-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "agent") || !strings.Contains(string(args), "stdio") {
		t.Fatalf("fake grok args = %s", args)
	}
}

func TestGrokACPProcessConfigurationChangesRequireRestart(t *testing.T) {
	process := &grokACPProcess{
		mcpURL:       "http://127.0.0.1/mcp",
		model:        "grok-4.5",
		effort:       "low",
		systemPrompt: "system",
		nativeSearch: false,
	}
	tests := []struct {
		name         string
		mcpURL       string
		model        string
		effort       string
		systemPrompt string
		nativeSearch bool
		wantMatch    bool
	}{
		{name: "unchanged", mcpURL: process.mcpURL, model: process.model, effort: process.effort, systemPrompt: process.systemPrompt, wantMatch: true},
		{name: "effort changed", mcpURL: process.mcpURL, model: process.model, effort: "high", systemPrompt: process.systemPrompt},
		{name: "native search changed", mcpURL: process.mcpURL, model: process.model, effort: process.effort, systemPrompt: process.systemPrompt, nativeSearch: true},
		{name: "model changed", mcpURL: process.mcpURL, model: "grok-5", effort: process.effort, systemPrompt: process.systemPrompt},
		{name: "system prompt changed", mcpURL: process.mcpURL, model: process.model, effort: process.effort, systemPrompt: "different"},
		{name: "MCP changed", mcpURL: "http://127.0.0.1/other", model: process.model, effort: process.effort, systemPrompt: process.systemPrompt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := process.matchesConfiguration(tt.mcpURL, tt.model, tt.effort, tt.systemPrompt, tt.nativeSearch); got != tt.wantMatch {
				t.Fatalf("matchesConfiguration() = %t, want %t", got, tt.wantMatch)
			}
		})
	}
}

func TestGrokBinProviderACPEphemeralDoesNotMutateState(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5-low", nil)
	p.sessionID = "persistent-session"
	p.messagesSent = 2
	p.commitGrokResult(
		Request{Ephemeral: true, Messages: []Message{UserText("temporary")}},
		grokCommandResult{sawEnd: true, sessionID: "temporary-session"},
	)
	if p.sessionID != "persistent-session" || p.messagesSent != 2 {
		t.Fatalf("ephemeral state = %q/%d, want persistent-session/2", p.sessionID, p.messagesSent)
	}
}

func TestGrokBinProviderACPErrorRedactsDiagnostics(t *testing.T) {
	const systemPrompt = "PRIVATE SYSTEM INSTRUCTION"
	const userPrompt = "PRIVATE USER QUESTION"
	const secret = "super-secret-value"
	p := NewGrokBinProvider("grok-4.5-low", map[string]string{"GROK_TEST_SECRET": secret})
	redact := p.grokACPDiagnosticRedactor(
		[]Message{SystemText(systemPrompt), UserText(userPrompt)},
		p.buildCommandEnv(),
	)
	diagnostic := redact("stderr " + systemPrompt + " " + userPrompt + " " + secret)
	if strings.Contains(diagnostic, systemPrompt) || strings.Contains(diagnostic, userPrompt) || strings.Contains(diagnostic, secret) {
		t.Fatalf("ACP diagnostics leaked private data: %s", diagnostic)
	}
	if got := strings.Count(diagnostic, "[redacted]"); got != 3 {
		t.Fatalf("ACP diagnostic redactions = %d, want 3: %s", got, diagnostic)
	}
}

func TestGrokBinProviderACPCancellationDuringInitialize(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	path := filepath.Join(binDir, "grok")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) sleep 30 ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := NewGrokBinProvider("grok-4.5-low", nil)
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{UserText("wait")}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- stream.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream cancellation did not interrupt Grok ACP initialize")
	}
}

func TestGrokBinProviderACPCancellationPreservesResidentSession(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	path := filepath.Join(binDir, "grok")
	script := `#!/bin/sh
prompt_id=""
while IFS= read -r line; do
  id=${line#*\"id\":}
  id=${id%%,*}
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{},\"authMethods\":[{\"id\":\"cached_token\",\"name\":\"Cached\"}]}}" ;;
    *'"method":"authenticate"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}" ;;
    *'"method":"session/new"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"sessionId\":\"cancel-session\"}}" ;;
    *'"method":"session/prompt"'*) prompt_id=$id ;;
    *'"method":"session/cancel"'*)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"cancel-session\",\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"late\"}}}}"
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$prompt_id,\"result\":{\"stopReason\":\"cancelled\",\"_meta\":{\"inputTokens\":10,\"outputTokens\":1,\"cachedReadTokens\":0,\"reasoningTokens\":0,\"totalTokens\":11}}}"
      ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := NewGrokBinProvider("grok-4.5-low", nil)
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{UserText("wait")}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.acpMu.Lock()
		ready := p.acpProcess != nil && p.acpProcess.sessionID == "cancel-session"
		p.acpMu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Grok ACP session did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- stream.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream cancellation did not settle Grok ACP prompt")
	}
	p.acpMu.Lock()
	process := p.acpProcess
	p.acpMu.Unlock()
	if process == nil || process.sessionID != "cancel-session" {
		t.Fatalf("cancelled ACP process = %+v, want resident cancel-session", process)
	}
	if p.sessionID != "cancel-session" || p.messagesSent != 1 {
		t.Fatalf("cancelled ACP provider state = %q/%d, want cancel-session/1", p.sessionID, p.messagesSent)
	}
	p.CleanupMCP()
}

func collectGrokACPTestText(t *testing.T, stream Stream) string {
	t.Helper()
	var text strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return text.String()
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		if event.Type == EventTextDelta {
			text.WriteString(event.Text)
		}
	}
}

func TestGrokBinProviderACPRealMultiTurnIsolationSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.6-low", nil)
	defer p.CleanupMCP()
	system := SystemText("Reply exactly as requested. Do not use tools.")
	firstUser := UserText("Reply with exactly: GROK TURN ONE OK")
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{system, firstUser}})
	if err != nil {
		t.Fatal(err)
	}
	first := collectGrokACPTestText(t, stream)
	if !strings.Contains(first, "GROK TURN ONE OK") {
		t.Fatalf("first response = %q", first)
	}
	firstSessionID := p.sessionID
	secondUser := UserText("Reply with exactly: GROK TURN TWO OK")
	stream, err = p.Stream(context.Background(), Request{Messages: []Message{system, firstUser, AssistantText(first), secondUser}})
	if err != nil {
		t.Fatal(err)
	}
	second := collectGrokACPTestText(t, stream)
	if !strings.Contains(second, "GROK TURN TWO OK") {
		t.Fatalf("second response = %q", second)
	}
	if p.sessionID == "" || p.sessionID != firstSessionID {
		t.Fatalf("Grok session changed across turns: %q -> %q", firstSessionID, p.sessionID)
	}

	var summaries, promptFiles []string
	err = filepath.WalkDir(filepath.Join(p.grokHome, "sessions"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		switch entry.Name() {
		case "summary.json":
			summaries = append(summaries, path)
		case "prompt_0.txt", "prompt_1.txt":
			promptFiles = append(promptFiles, path)
		case "chat_history.jsonl":
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, forbidden := range []string{"The following skills are available for use", "bundled/skills/review/SKILL.md", "Read this file with read_file before responding"} {
				if strings.Contains(string(raw), forbidden) {
					t.Errorf("Grok history contains ambient prompt contamination %q", forbidden)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || len(promptFiles) != 0 {
		t.Fatalf("Grok artifacts: summaries=%v promptFiles=%v", summaries, promptFiles)
	}
}

type grokACPReviewProbeTool struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (t *grokACPReviewProbeTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "spawn_agent",
		Description: "Launch one requested reviewer. Call once per requested reviewer model.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_name": map[string]any{"type": "string", "enum": []string{"reviewer"}},
				"model":      map[string]any{"type": "string"},
				"prompt":     map[string]any{"type": "string"},
			},
			"required": []string{"agent_name", "model", "prompt"},
		},
	}
}

func (t *grokACPReviewProbeTool) Execute(_ context.Context, args json.RawMessage) (ToolOutput, error) {
	var call map[string]any
	if err := json.Unmarshal(args, &call); err != nil {
		return ToolOutput{}, err
	}
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	return TextOutput("REVIEW_COMPLETE"), nil
}

func (t *grokACPReviewProbeTool) Preview(json.RawMessage) string { return "review probe" }

func TestGrokBinProviderACPRealMultiReviewOrchestrationSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.6-low", nil)
	defer p.CleanupMCP()
	tool := &grokACPReviewProbeTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(p, registry)
	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{
			SystemText("Use only the supplied term-llm tools. Do not activate skills or read workflow files. When multiple reviews are requested, call spawn_agent once for each requested model before replying."),
			UserText("Review with opus and gpt. Use reviewer agents with models claude-bin:opus-max and chatgpt:gpt-5.6-sol-high. Call both now, then reply REVIEWERS LAUNCHED."),
		},
		Tools:    []ToolSpec{tool.Spec()},
		MaxTurns: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := collectGrokACPTestText(t, stream)
	tool.mu.Lock()
	calls := append([]map[string]any(nil), tool.calls...)
	tool.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("spawn_agent calls = %d (%v), response=%q", len(calls), calls, text)
	}
	models := map[string]bool{}
	for _, call := range calls {
		model, _ := call["model"].(string)
		models[model] = true
	}
	for _, model := range []string{"claude-bin:opus-max", "chatgpt:gpt-5.6-sol-high"} {
		if !models[model] {
			t.Fatalf("spawn_agent models = %v, missing %q", models, model)
		}
	}
	if !strings.Contains(text, "REVIEWERS LAUNCHED") {
		t.Fatalf("multi-review response = %q", text)
	}
}

func TestGrokBinProviderACPRealOversizedVerbatimSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.6-low", nil)
	defer p.CleanupMCP()
	prompt := strings.Repeat("context filler ", 1900) + "\nReply with exactly: OVERSIZED VERBATIM OK"
	if len(prompt) <= 25_000 {
		t.Fatalf("test prompt is only %d bytes", len(prompt))
	}
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{
		SystemText("Ignore repetitive filler and follow the final response instruction exactly. Do not use tools."),
		UserText(prompt),
	}})
	if err != nil {
		t.Fatal(err)
	}
	answer := collectGrokACPTestText(t, stream)
	if !strings.Contains(answer, "OVERSIZED VERBATIM OK") {
		t.Fatalf("oversized response = %q", answer)
	}
	var promptFiles []string
	err = filepath.WalkDir(filepath.Join(p.grokHome, "sessions"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.HasPrefix(entry.Name(), "prompt_") && strings.HasSuffix(entry.Name(), ".txt") {
			promptFiles = append(promptFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(promptFiles) != 0 {
		t.Fatalf("verbatim prompt unexpectedly offloaded: %v", promptFiles)
	}
}

func grokUpdateOccurrenceCount(home, needle string) int {
	count := 0
	_ = filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != "updates.jsonl" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr == nil {
			count += strings.Count(string(raw), needle)
		}
		return nil
	})
	return count
}

func waitForGrokUpdateAfter(t *testing.T, home, needle string, before int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if grokUpdateOccurrenceCount(home, needle) > before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Grok updates did not add %q within %s", needle, timeout)
}

func TestGrokBinProviderACPRealCancellationRecoverySmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.6-high", nil)
	defer p.CleanupMCP()
	system := SystemText("Follow the user's response format exactly. Do not use tools.")
	firstUser := UserText("Reply with exactly: CANCELLATION BASELINE OK")
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{system, firstUser}})
	if err != nil {
		t.Fatal(err)
	}
	first := collectGrokACPTestText(t, stream)
	if !strings.Contains(first, "CANCELLATION BASELINE OK") {
		t.Fatalf("baseline response = %q", first)
	}
	stableSessionID := p.sessionID

	cancelText := "Remember this cancellation marker: WIDGET-8823. Then analyze every possible ordering of the first 20 prime numbers in exhaustive detail."
	cancelUser := UserText(cancelText)
	thoughtsBefore := grokUpdateOccurrenceCount(p.grokHome, `"sessionUpdate":"agent_thought_chunk"`)
	stream, err = p.Stream(context.Background(), Request{Messages: []Message{system, firstUser, AssistantText(first), cancelUser}})
	if err != nil {
		t.Fatal(err)
	}
	waitForGrokUpdateAfter(t, p.grokHome, `"sessionUpdate":"agent_thought_chunk"`, thoughtsBefore, 15*time.Second)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	// channelStream.Close waits for the provider producer to finish, so state
	// committed by the cancelled prompt is synchronized before these reads.
	if p.sessionID != stableSessionID || p.messagesSent != 4 {
		t.Fatalf("cancelled state = %q/%d, want %q/4", p.sessionID, p.messagesSent, stableSessionID)
	}
	p.acpMu.Lock()
	resident := p.acpProcess
	p.acpMu.Unlock()
	if resident == nil || resident.sessionID != stableSessionID {
		t.Fatalf("cancelled resident process = %+v", resident)
	}

	followUp := UserText("What cancellation marker did I give you? Reply with exactly: WIDGET-8823")
	stream, err = p.Stream(context.Background(), Request{Messages: []Message{system, firstUser, AssistantText(first), cancelUser, followUp}})
	if err != nil {
		t.Fatal(err)
	}
	answer := collectGrokACPTestText(t, stream)
	if !strings.Contains(answer, "WIDGET-8823") || p.sessionID != stableSessionID {
		t.Fatalf("recovery response/session = %q / %q, want %q", answer, p.sessionID, stableSessionID)
	}
}

func TestGrokBinProviderACPRealSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.5-low", nil)
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{
		SystemText("Reply concisely and do not use tools."),
		UserText("Reply with exactly: REAL ACP OK"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var usage *Usage
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		if event.Type == EventTextDelta {
			text += event.Text
		}
		if event.Type == EventUsage {
			usage = event.Use
		}
	}
	if !strings.Contains(text, "REAL ACP OK") {
		t.Fatalf("text = %q", text)
	}
	if usage == nil || usage.ProviderRawInputTokens <= 0 || usage.OutputTokens <= 0 || usage.ProviderTotalTokens <= 0 {
		t.Fatalf("usage = %+v", usage)
	}
}

type grokDynamicResultProbeTool struct {
	mu    sync.Mutex
	calls int
}

func (t *grokDynamicResultProbeTool) Spec() ToolSpec {
	return ToolSpec{Name: "late_dynamic", Description: "Returns DYNAMIC_TOOL_OK after dynamic registration.", Schema: map[string]any{"type": "object"}}
}
func (t *grokDynamicResultProbeTool) Execute(context.Context, json.RawMessage) (ToolOutput, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return TextOutput("DYNAMIC_TOOL_OK"), nil
}
func (t *grokDynamicResultProbeTool) Preview(json.RawMessage) string { return "dynamic result probe" }

type grokDynamicActivationProbeTool struct {
	mu     sync.Mutex
	calls  int
	engine *Engine
	late   Tool
}

func (t *grokDynamicActivationProbeTool) Spec() ToolSpec {
	return ToolSpec{Name: "activate_dynamic", Description: "Registers the late_dynamic tool. Call this before searching for late_dynamic.", Schema: map[string]any{"type": "object"}}
}
func (t *grokDynamicActivationProbeTool) Execute(context.Context, json.RawMessage) (ToolOutput, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	t.engine.AddDynamicTool(t.late)
	return TextOutput("late_dynamic is registered; discover and call it now. Do not claim its result without calling it."), nil
}
func (t *grokDynamicActivationProbeTool) Preview(json.RawMessage) string {
	return "register dynamic result probe"
}

func TestGrokBinProviderACPRealDynamicToolSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP dynamic-tool smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	provider := NewGrokBinProvider("grok-4.6-low", nil)
	defer provider.CleanupMCP()
	registry := NewToolRegistry()
	late := &grokDynamicResultProbeTool{}
	activation := &grokDynamicActivationProbeTool{late: late}
	registry.Register(activation)
	engine := NewEngine(provider, registry)
	activation.engine = engine

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{
			SystemText("Use only supplied term-llm MCP tools. You must execute every requested tool and must not invent tool results."),
			UserText("First call activate_dynamic exactly once. After it returns, search again for the newly registered late_dynamic tool and call late_dynamic exactly once in this same turn. Do not reply until both real tool calls have completed; then reply with the late_dynamic result."),
		},
		Tools:    []ToolSpec{activation.Spec()},
		MaxTurns: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := collectGrokACPTestText(t, stream)
	activation.mu.Lock()
	activationCalls := activation.calls
	activation.mu.Unlock()
	late.mu.Lock()
	calls := late.calls
	late.mu.Unlock()
	if activationCalls != 1 || calls != 1 || !strings.Contains(text, "DYNAMIC_TOOL_OK") {
		t.Fatalf("activate_dynamic calls=%d late_dynamic calls=%d response=%q", activationCalls, calls, text)
	}
}

func TestGrokBinProviderACPRealToolSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP tool smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.5-low", nil)
	p.SetToolExecutor(func(context.Context, string, json.RawMessage) (ToolOutput, error) {
		return TextOutput("ECHO_TOOL_OK"), nil
	})
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{
			SystemText("Use only the supplied term-llm tool when asked."),
			UserText("Call the echo_once tool exactly once, then reply with its result."),
		},
		Tools: []ToolSpec{{
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
	var usage *Usage
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		switch event.Type {
		case EventToolCall:
			toolCalls++
			if event.ToolName != "echo_once" || event.ToolResponse == nil {
				t.Fatalf("tool event = %+v", event)
			}
			event.ToolResponse <- ToolExecutionResponse{Result: TextOutput("ECHO_TOOL_OK")}
		case EventTextDelta:
			text += event.Text
		case EventUsage:
			usage = event.Use
		}
	}
	if toolCalls != 1 || !strings.Contains(text, "ECHO_TOOL_OK") || usage == nil {
		t.Fatalf("tool calls=%d text=%q usage=%+v", toolCalls, text, usage)
	}
}

func TestGrokBinProviderACPRealNativeSearchSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP native search smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.5-low", nil)
	defer p.CleanupMCP()
	stream, err := p.Stream(context.Background(), Request{
		Search: true,
		Messages: []Message{
			SystemText("Use native X Search and answer concisely. Do not guess."),
			UserText("Find the official @SpaceXAI post announcing Grok 4.5 and return its direct x.com URL."),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var usage *Usage
	nativeSearchStarts := 0
	nativeSearchEnds := 0
	var nativeSearchArgs json.RawMessage
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		switch event.Type {
		case EventTextDelta:
			text += event.Text
		case EventUsage:
			usage = event.Use
		case EventToolExecStart:
			if strings.HasPrefix(event.ToolName, "x_") {
				nativeSearchStarts++
				nativeSearchArgs = event.ToolArgs
			}
		case EventToolExecEnd:
			if strings.HasPrefix(event.ToolName, "x_") && event.ToolSuccess {
				nativeSearchEnds++
			}
		}
	}
	if !strings.Contains(text, "https://x.com/SpaceXAI/status/2074915721684086811") {
		t.Fatalf("native X Search response = %q", text)
	}
	if nativeSearchStarts == 0 || nativeSearchStarts != nativeSearchEnds || !strings.Contains(string(nativeSearchArgs), `"query"`) {
		t.Fatalf("native X Search events starts=%d ends=%d args=%s", nativeSearchStarts, nativeSearchEnds, nativeSearchArgs)
	}
	if usage == nil || usage.ProviderRawInputTokens <= 0 || usage.OutputTokens <= 0 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestGrokACPSessionMetaUsesDocumentedSystemPromptExtension(t *testing.T) {
	meta, err := grokACPSessionMeta("private system")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(meta, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["systemPromptOverride"] != "private system" {
		t.Fatalf("session metadata = %s", meta)
	}
}

func TestGrokBinProviderACPRealMixedTransportSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed mixed-transport smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.5-low", nil)
	defer p.CleanupMCP()
	drainText := func(messages []Message) string {
		t.Helper()
		stream, err := p.Stream(context.Background(), Request{Messages: messages})
		if err != nil {
			t.Fatal(err)
		}
		var text string
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				return text
			}
			if err != nil {
				t.Fatal(err)
			}
			if event.Type == EventError {
				t.Fatal(event.Err)
			}
			if event.Type == EventTextDelta {
				text += event.Text
			}
		}
	}

	messages := []Message{SystemText("Reply briefly."), UserText("Reply with FIRST.")}
	first := drainText(messages)
	if !strings.Contains(first, "FIRST") {
		t.Fatalf("first ACP response = %q", first)
	}
	messages = append(messages, AssistantText(first), Message{Role: RoleUser, Parts: []Part{
		{Type: PartText, Text: "Briefly acknowledge this image."},
		{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/png", Base64: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="}},
	}})
	imageReply := drainText(messages)
	if strings.TrimSpace(imageReply) == "" {
		t.Fatal("legacy image fallback returned no text")
	}
	messages = append(messages, AssistantText(imageReply), UserText("Reply with THIRD."))
	third := drainText(messages)
	if !strings.Contains(third, "THIRD") {
		t.Fatalf("ACP response after legacy image turn = %q", third)
	}
	if p.sessionID == "" || p.messagesSent != len(messages) {
		t.Fatalf("mixed transport state = %q/%d", p.sessionID, p.messagesSent)
	}
}

func TestGrokACPHTTPServerPayload(t *testing.T) {
	server := grokACPMCPServer("http://127.0.0.1:1234/mcp", "secret")
	if server.Type != "http" || server.Name != "term-llm" || server.URL == "" || len(server.Headers) != 1 || server.Headers[0].Value != "Bearer secret" {
		t.Fatalf("MCP server = %+v", server)
	}
	_ = acp.ProtocolVersion1 // Keep this test tied to the generic ACP package.
}
