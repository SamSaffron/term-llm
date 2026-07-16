package llm

import (
	"context"
	"encoding/json"
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
	if !strings.Contains(text, "tools:\n  - search_tool\n  - use_tool") || strings.Contains(text, "  - web_search") || strings.Contains(text, "  - web_fetch") || strings.Contains(text, "  - x_search") || strings.Contains(text, "list_dir") {
		t.Fatalf("unsafe agent profile:\n%s", text)
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
	binDir := t.TempDir()
	writeFakeGrokACP(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := NewGrokBinProvider("grok-4.5-low", nil)
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

func TestGrokBinProviderACPRestartsLoadsAndSuppressesReplay(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	writeFakeGrokACP(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := NewGrokBinProvider("grok-4.5-low", nil)
	defer p.CleanupMCP()
	firstMessages := []Message{SystemText("system"), UserText("first")}
	drain := func(messages []Message) []Event {
		t.Helper()
		stream, err := p.Stream(context.Background(), Request{Messages: messages})
		if err != nil {
			t.Fatal(err)
		}
		var events []Event
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				return events
			}
			if err != nil {
				t.Fatal(err)
			}
			if event.Type == EventError {
				t.Fatal(event.Err)
			}
			events = append(events, event)
		}
	}
	_ = drain(firstMessages)

	p.acpMu.Lock()
	p.stopGrokACPProcess(p.acpProcess)
	p.acpProcess = nil
	p.acpMu.Unlock()

	secondMessages := append(append([]Message(nil), firstMessages...), AssistantText("answer"), UserText("second"))
	for _, event := range drain(secondMessages) {
		if strings.Contains(event.Text, "replayed history") {
			t.Fatalf("session/load replay leaked into stream: %+v", event)
		}
	}
	if p.messagesSent != len(secondMessages) || p.sessionID != "fake-session" {
		t.Fatalf("state after restart = %q/%d", p.sessionID, p.messagesSent)
	}
	launches, err := os.ReadFile(filepath.Join(p.grokHome, "fake-launches"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(launches), "launch\n"); got != 2 {
		t.Fatalf("ACP process launches = %d, want 2", got)
	}
}

func TestGrokBinProviderACPRestartsWhenEffortChanges(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	writeFakeGrokACP(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := NewGrokBinProvider("grok-4.5-low", nil)
	defer p.CleanupMCP()
	drain := func(req Request) {
		t.Helper()
		stream, err := p.Stream(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if event.Type == EventError {
				t.Fatal(event.Err)
			}
		}
	}
	messages := []Message{SystemText("system"), UserText("first")}
	drain(Request{Messages: messages})
	messages = append(messages, AssistantText("answer"), UserText("second"))
	drain(Request{Messages: messages, ReasoningEffort: "high"})

	launches, err := os.ReadFile(filepath.Join(p.grokHome, "fake-launches"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(launches), "launch\n"); got != 2 {
		t.Fatalf("ACP process launches = %d, want 2", got)
	}
	args, err := os.ReadFile(filepath.Join(p.grokHome, "fake-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--reasoning-effort high") {
		t.Fatalf("restarted ACP args = %s", args)
	}
}

func TestGrokBinProviderACPRestartsWhenNativeSearchChanges(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	writeFakeGrokACP(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := NewGrokBinProvider("grok-4.5-low", nil)
	defer p.CleanupMCP()
	drain := func(req Request) {
		t.Helper()
		stream, err := p.Stream(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if event.Type == EventError {
				t.Fatal(event.Err)
			}
		}
	}
	messages := []Message{SystemText("system"), UserText("first")}
	drain(Request{Messages: messages})
	messages = append(messages, AssistantText("answer"), UserText("search X"))
	drain(Request{Messages: messages, Search: true})

	launches, err := os.ReadFile(filepath.Join(p.grokHome, "fake-launches"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(launches), "launch\n"); got != 2 {
		t.Fatalf("ACP process launches = %d, want 2", got)
	}
	args, err := os.ReadFile(filepath.Join(p.grokHome, "fake-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--tools "+grokNativeSearchToolAllowlist) {
		t.Fatalf("restarted ACP args = %s", args)
	}
}

func TestGrokBinProviderACPEphemeralDoesNotMutateState(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	writeFakeGrokACP(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := NewGrokBinProvider("grok-4.5-low", nil)
	defer p.CleanupMCP()
	drain := func(request Request) {
		t.Helper()
		stream, err := p.Stream(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if event.Type == EventError {
				t.Fatal(event.Err)
			}
		}
	}
	persistentMessages := []Message{SystemText("system"), UserText("first")}
	drain(Request{Messages: persistentMessages})
	wantSession, wantSent := p.sessionID, p.messagesSent
	drain(Request{Ephemeral: true, Messages: []Message{UserText("temporary")}})
	if p.sessionID != wantSession || p.messagesSent != wantSent {
		t.Fatalf("ephemeral state = %q/%d, want %q/%d", p.sessionID, p.messagesSent, wantSession, wantSent)
	}
	p.acpMu.Lock()
	process := p.acpProcess
	p.acpMu.Unlock()
	if process != nil {
		t.Fatal("ephemeral process was retained as conversation process")
	}
}

func TestGrokBinProviderACPErrorRedactsDiagnostics(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	path := filepath.Join(binDir, "grok")
	const systemPrompt = "PRIVATE SYSTEM INSTRUCTION"
	const userPrompt = "PRIVATE USER QUESTION"
	const secret = "super-secret-value"
	script := "#!/bin/sh\necho 'stderr " + systemPrompt + " " + userPrompt + " " + secret + "' >&2\nexit 7\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := NewGrokBinProvider("grok-4.5-low", map[string]string{"GROK_TEST_SECRET": secret})
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{SystemText(systemPrompt), UserText(userPrompt)}})
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			streamErr = event.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected ACP process error")
	}
	diagnostic := streamErr.Error()
	if strings.Contains(diagnostic, systemPrompt) || strings.Contains(diagnostic, userPrompt) || strings.Contains(diagnostic, secret) {
		t.Fatalf("ACP diagnostics leaked private data: %s", diagnostic)
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

func TestGrokBinProviderACPCancellationStopsProcess(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	path := filepath.Join(binDir, "grok")
	script := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{},\"authMethods\":[{\"id\":\"cached_token\",\"name\":\"Cached\"}]}}" ;;
    *'"method":"authenticate"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}" ;;
    *'"method":"session/new"'*) printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"sessionId\":\"cancel-session\"}}" ;;
    *'"method":"session/prompt"'*) sleep 30 ;;
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
	done := make(chan error, 1)
	go func() { done <- stream.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream cancellation did not stop Grok ACP process")
	}
	p.acpMu.Lock()
	process := p.acpProcess
	p.acpMu.Unlock()
	if process != nil {
		t.Fatal("cancelled ACP process remained attached to provider")
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

func writeFakeGrokACP(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "grok")
	script := `#!/bin/sh
printf '%s\n' "$*" > "$GROK_HOME/fake-args"
printf 'launch\n' >> "$GROK_HOME/fake-launches"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{\"loadSession\":true,\"promptCapabilities\":{\"image\":true},\"mcpCapabilities\":{\"http\":true}},\"authMethods\":[{\"id\":\"cached_token\",\"name\":\"Cached\"}]}}"
      ;;
    *'"method":"authenticate"'*)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"sessionId\":\"fake-session\"}}"
      ;;
    *'"method":"session/load"'*)
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fake-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replayed history"}}}}'
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
      ;;
    *'"method":"session/prompt"'*)
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fake-session","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fake-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"answer"}}}}'
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"stopReason\":\"end_turn\",\"_meta\":{\"inputTokens\":100,\"outputTokens\":20,\"cachedReadTokens\":92,\"reasoningTokens\":7,\"totalTokens\":121}}}"
      ;;
    *'"method":"session/cancel"'*) ;;
    *'"method":"session/close"'*)
      printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
      ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
