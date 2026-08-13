package llm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCursorBinProviderCapabilities(t *testing.T) {
	caps := NewCursorBinProvider("auto-smart", nil).Capabilities()
	if !caps.ToolCalls || !caps.ManagesOwnContext || !caps.InlineToolLoop || !caps.OrderedInlineToolEvents {
		t.Fatalf("capabilities = %+v, want tool calls, managed context, inline loop, and ordered inline tool events", caps)
	}
}

func TestParseCursorModel(t *testing.T) {
	tests := []struct {
		input, model, effort string
		fast                 bool
	}{
		{"grok-4.5", "grok-4.5", "", false},
		{"grok-4.5-low", "grok-4.5", "low", false},
		{"grok-4.5-high-fast", "grok-4.5", "high", true},
		{"composer-2.5-fast", "composer-2.5", "", true},
		{"composer-2.5-extra-high", "composer-2.5", "extra-high", false},
		{"composer-2.5-extra-high-fast", "composer-2.5", "extra-high", true},
	}
	for _, tt := range tests {
		model, effort, fast := parseCursorModel(tt.input)
		if model != tt.model || effort != tt.effort || fast != tt.fast {
			t.Fatalf("parseCursorModel(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.input, model, effort, fast, tt.model, tt.effort, tt.fast)
		}
	}
}

func TestCursorModelArgument(t *testing.T) {
	tests := map[string]string{
		"auto-smart":              "auto",
		"grok-4.5":                "cursor-grok-4.5-high",
		"grok-4.5-fast":           "cursor-grok-4.5-high-fast",
		"grok-4.5-low":            "cursor-grok-4.5-low",
		"grok-4.5-high-fast":      "cursor-grok-4.5-high-fast",
		"grok-4.6":                "grok-4.6",
		"grok-4.6-fast":           "grok-4.6-fast",
		"grok-4.6-high":           "cursor-grok-4.6-high",
		"grok-4.6-high-fast":      "cursor-grok-4.6-high-fast",
		"composer-2.5":            "composer-2.5",
		"composer-2.5-extra-high": "composer-2.5-extra-high",
	}
	for input, want := range tests {
		model, effort, fast := parseCursorModel(input)
		if got := cursorModelArgument(model, effort, fast); got != want {
			t.Fatalf("cursorModelArgument(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCursorArgsRestrictToolsToMCP(t *testing.T) {
	home := t.TempDir()
	p := NewCursorBinProvider("grok-4.5-low", nil)
	args := p.buildCursorArgs(Request{}, "session-1", nil, home)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--print", "--output-format stream-json", "--stream-partial-output",
		"--allowed-tools mcp_tool_call,get_mcp_tools_tool_call", "--force",
		"--resume session-1", "--model cursor-grok-4.5-low",
		"--workspace " + filepath.Join(home, "cwd"),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Cursor args %q missing %q", joined, want)
		}
	}
}

func TestCursorArgsServiceTierMapsToFastSuffix(t *testing.T) {
	home := t.TempDir()
	p := NewCursorBinProvider("grok-4.5-high", nil)

	for _, tier := range []string{"fast", ServiceTierFast, "FAST", "Priority"} {
		args := p.buildCursorArgs(Request{ServiceTier: tier, ServiceTierSet: true}, "", nil, home)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--model cursor-grok-4.5-high-fast") {
			t.Fatalf("ServiceTier %q args %q missing -fast model suffix", tier, joined)
		}
	}

	args := p.buildCursorArgs(Request{ServiceTierSet: true}, "", nil, home)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-fast") {
		t.Fatalf("cleared ServiceTier unexpectedly kept -fast: %q", joined)
	}
	if !strings.Contains(joined, "--model cursor-grok-4.5-high") {
		t.Fatalf("cleared ServiceTier args %q missing base model", joined)
	}
}

func TestCursorMCPConfigUsesPrivateProjectAndBearerToken(t *testing.T) {
	home := t.TempDir()
	if err := ensureCursorHomeLayout(home); err != nil {
		t.Fatal(err)
	}
	p := NewCursorBinProvider("auto-smart", nil)
	p.mcpURL = "http://127.0.0.1:1234/mcp"
	p.mcpToken = "mcp-secret"
	if err := p.writeCursorMCPConfig(home, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "cwd", ".cursor", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"term-llm"`, p.mcpURL, "Bearer mcp-secret"} {
		if !strings.Contains(text, want) {
			t.Fatalf("MCP config %s missing %q", text, want)
		}
	}
}

func TestHandleCursorStreamLineMapsDeltasAndUsage(t *testing.T) {
	events := make(chan Event, 4)
	send := eventSender{ctx: context.Background(), ch: events}
	state := cursorStreamState{}
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"cursor-session"}`,
		`{"type":"thinking","subtype":"delta","text":"thought"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]},"timestamp_ms":1}`,
		`{"type":"result","subtype":"success","session_id":"cursor-session","usage":{"inputTokens":10,"outputTokens":3,"cacheReadTokens":2}}`,
	}
	for _, line := range lines {
		if err := handleCursorStreamLine(line, send, &state); err != nil {
			t.Fatal(err)
		}
	}
	if event := <-events; event.Type != EventReasoningDelta || event.Text != "thought" {
		t.Fatalf("reasoning event = %#v", event)
	}
	if event := <-events; event.Type != EventTextDelta || event.Text != "answer" {
		t.Fatalf("text event = %#v", event)
	}
	if state.sessionID != "cursor-session" || !state.sawResult || state.usage == nil {
		t.Fatalf("stream state = %#v", state)
	}
	if state.usage.InputTokens != 10 || state.usage.OutputTokens != 3 || state.usage.CachedInputTokens != 2 {
		t.Fatalf("usage = %#v", state.usage)
	}
}

func TestHandleCursorStreamLineSkipsBufferedAndFinalDuplicateText(t *testing.T) {
	events := make(chan Event, 4)
	send := eventSender{ctx: context.Background(), ch: events}
	state := cursorStreamState{}
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]},"timestamp_ms":1}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]},"timestamp_ms":2,"model_call_id":""}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]},"timestamp_ms":3,"model_call_id":null}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]}}`,
		`{"type":"result","subtype":"success","result":"answer"}`,
	}
	for _, line := range lines {
		if err := handleCursorStreamLine(line, send, &state); err != nil {
			t.Fatal(err)
		}
	}

	if event := <-events; event.Type != EventTextDelta || event.Text != "answer" {
		t.Fatalf("text event = %#v", event)
	}
	select {
	case event := <-events:
		t.Fatalf("unexpected duplicate event = %#v", event)
	default:
	}
	if !state.sawResult {
		t.Fatal("result event was not recorded")
	}
}

func TestCursorCommandEnvSharesAuthConfigAndIsolatesData(t *testing.T) {
	home := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", configDir)
	t.Setenv("CURSOR_API_KEY", "")
	p := NewCursorBinProvider("auto-smart", map[string]string{
		"CURSOR_DATA_DIR": "/unsafe/data",
		"CURSOR_API_KEY":  "secret-cursor-key",
	})
	env := strings.Join(p.buildCommandEnv(home), "\n")
	for _, want := range []string{
		"CURSOR_CONFIG_DIR=" + configDir,
		"CURSOR_DATA_DIR=" + filepath.Join(home, "data"),
		"CURSOR_API_KEY=secret-cursor-key",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("command environment missing %q\n%s", want, env)
		}
	}
	if strings.Contains(env, filepath.Join(home, "config")) {
		t.Fatalf("command environment isolated auth config into private home: %s", env)
	}
	if strings.Contains(env, "/unsafe/") {
		t.Fatalf("command environment retained unsafe Cursor data override: %s", env)
	}
	if got := p.cursorDiagnosticRedactor()("failed with secret-cursor-key"); strings.Contains(got, "secret-cursor-key") {
		t.Fatalf("diagnostic was not redacted: %q", got)
	}
}

func TestCursorBinHasCredentialsReadsAuthInfo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", dir)
	t.Setenv("CURSOR_API_KEY", "")
	if CursorBinHasCredentials() {
		t.Fatal("expected no credentials without cli-config authInfo or API key")
	}
	if err := os.WriteFile(filepath.Join(dir, "cli-config.json"), []byte(`{"permissions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if CursorBinHasCredentials() {
		t.Fatal("expected no credentials when authInfo is missing")
	}
	if err := os.WriteFile(filepath.Join(dir, "cli-config.json"), []byte(`{"authInfo":{"email":"a@b.c"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !CursorBinHasCredentials() {
		t.Fatal("expected credentials when authInfo is present")
	}
	t.Setenv("CURSOR_API_KEY", "key-from-env")
	_ = os.Remove(filepath.Join(dir, "cli-config.json"))
	if !CursorBinHasCredentials() {
		t.Fatal("expected credentials from CURSOR_API_KEY")
	}
}

func TestDispatchCursorEventsDrainsStdoutBeforeToolRequest(t *testing.T) {
	oldGrace := cursorToolDrainGrace
	cursorToolDrainGrace = 30 * time.Millisecond
	defer func() { cursorToolDrainGrace = oldGrace }()

	lineCh := make(chan string, 2)
	toolReqCh := make(chan cliToolRequest, 1)
	events := make(chan Event, 4)
	send := eventSender{ctx: context.Background(), ch: events}
	state := &cursorStreamState{reasoningItem: 1}

	lineCh <- `{"type":"assistant","message":{"content":[{"type":"text","text":"before-tool"}]},"timestamp_ms":1}`
	resp := make(chan ToolExecutionResponse, 1)
	ack := make(chan error, 1)
	toolReqCh <- cliToolRequest{
		ctx:      context.Background(),
		callID:   "mcp-echo-1",
		name:     "echo",
		args:     json.RawMessage(`{}`),
		response: resp,
		ack:      ack,
	}

	done := make(chan error, 1)
	go func() {
		done <- (&CursorBinProvider{}).dispatchCursorEvents(context.Background(), lineCh, toolReqCh, send, state)
	}()

	// Allow the dispatcher to see the tool request, then deliver a late stdout line
	// that must still be drained before the tool call event is emitted.
	time.Sleep(5 * time.Millisecond)
	lineCh <- `{"type":"assistant","message":{"content":[{"type":"text","text":"late-line"}]},"timestamp_ms":2}`
	close(lineCh)

	if err := <-done; err != nil {
		t.Fatalf("dispatchCursorEvents: %v", err)
	}
	if event := <-events; event.Type != EventTextDelta || event.Text != "before-tool" {
		t.Fatalf("first event = %#v, want before-tool text", event)
	}
	if event := <-events; event.Type != EventTextDelta || event.Text != "late-line" {
		t.Fatalf("second event = %#v, want late-line text before tool call", event)
	}
	if event := <-events; event.Type != EventToolCall || event.ToolName != "echo" {
		t.Fatalf("third event = %#v, want tool call", event)
	}
	if err := <-ack; err != nil {
		t.Fatalf("tool ack: %v", err)
	}
}
