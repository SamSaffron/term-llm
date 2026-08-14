package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGrokBinProviderPrepareCommandUsesWorkingDir(t *testing.T) {
	provider := NewGrokBinProvider("grok-4.5", nil)
	workingDir := t.TempDir()

	cmd, cleanup, err := provider.prepareGrokCommand(context.Background(), []string{"--version"}, workingDir)
	if err != nil {
		t.Fatalf("prepareGrokCommand: %v", err)
	}
	defer cleanup()

	if cmd.Dir != workingDir {
		t.Fatalf("Dir = %q, want %q", cmd.Dir, workingDir)
	}
	if cmd.WaitDelay != grokCommandWaitDelay {
		t.Fatalf("WaitDelay = %v, want %v", cmd.WaitDelay, grokCommandWaitDelay)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("expected grok subprocess to run in a detached session")
	}
}

func TestGrokBinProviderStreamSeparatesProcessAndCLICWD(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "grok"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake grok: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workingDir := t.TempDir()
	provider := NewGrokBinProvider("grok-4.5", nil)
	provider.grokHome = t.TempDir()
	args, _, err := provider.buildArgs(Request{Messages: []Message{UserText("hello")}}, filepath.Join(provider.grokHome, "prompt.json"))
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	cmd, cleanup, err := provider.prepareGrokCommand(context.Background(), args, workingDir)
	if err != nil {
		t.Fatalf("prepareGrokCommand: %v", err)
	}
	defer cleanup()
	if cmd.Dir != workingDir {
		t.Fatalf("process cwd = %q, want %q", cmd.Dir, workingDir)
	}
	wantCLICWD := filepath.Join(provider.grokHome, "cwd")
	if got := argValue(cmd.Args[1:], "--cwd"); got != wantCLICWD {
		t.Fatalf("Grok --cwd = %q, want neutral cwd %q", got, wantCLICWD)
	}
	if wantCLICWD == workingDir {
		t.Fatalf("neutral Grok cwd unexpectedly equals process cwd %q", workingDir)
	}
}

func TestParseGrokEffort(t *testing.T) {
	tests := []struct {
		model      string
		wantModel  string
		wantEffort string
	}{
		{model: "grok-4.6-high", wantModel: "grok-4.6", wantEffort: "high"},
		{model: "grok-4.6-xhigh", wantModel: "grok-4.6", wantEffort: "xhigh"},
		{model: "grok-4.5-high", wantModel: "grok-4.5", wantEffort: "high"},
		{model: "grok-4.5-xhigh", wantModel: "grok-4.5", wantEffort: "xhigh"},
		{model: "custom-model-max", wantModel: "custom-model", wantEffort: "max"},
		{model: "grok-composer-2.5-fast", wantModel: "grok-composer-2.5-fast"},
		{model: "future-model", wantModel: "future-model"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			model, effort := parseGrokEffort(tt.model)
			if model != tt.wantModel || effort != tt.wantEffort {
				t.Fatalf("parseGrokEffort(%q) = (%q, %q), want (%q, %q)", tt.model, model, effort, tt.wantModel, tt.wantEffort)
			}
		})
	}
}

func TestValidateGrokBinModel(t *testing.T) {
	for _, model := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if err := ValidateGrokBinModel(model); err == nil {
			t.Fatalf("ValidateGrokBinModel(%q) returned nil", model)
		}
	}
	for _, model := range []string{"", "grok-4.6", "grok-4.6-high", "grok-4.5", "grok-4.5-high", "subscription-custom-model"} {
		if err := ValidateGrokBinModel(model); err != nil {
			t.Fatalf("ValidateGrokBinModel(%q): %v", model, err)
		}
	}
}

func TestGrokBinProviderCapabilities(t *testing.T) {
	caps := NewGrokBinProvider("grok-4.5", nil).Capabilities()
	if !caps.NativeWebSearch || !caps.NativeWebFetch || !caps.ToolCalls || !caps.ManagesOwnContext || !caps.InlineToolLoop || !caps.OrderedInlineToolEvents {
		t.Fatalf("capabilities = %+v, want native search/fetch, tool calls, managed context, inline loop, and ordered inline tool events", caps)
	}
}

func TestGrokBinProviderBuildArgsDisablesNativeTools(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5", nil)
	p.grokHome = t.TempDir()

	args, _, err := p.buildArgs(Request{}, filepath.Join(p.grokHome, "prompt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := argValue(args, "--disallowed-tools"); got != grokDisallowedNativeTools {
		t.Fatalf("--disallowed-tools = %q, want %q", got, grokDisallowedNativeTools)
	}
	if got := argValue(args, "--tools"); got != "" {
		t.Fatalf("unexpected --tools allowlist %q", got)
	}
}

func TestGrokBinProviderBuildArgsEnablesNativeWebAndXSearch(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5", nil)
	p.grokHome = t.TempDir()

	args, _, err := p.buildArgs(Request{Search: true}, filepath.Join(p.grokHome, "prompt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := argValue(args, "--tools"); got != "search_tool,use_tool,web_search,web_fetch,x_search" {
		t.Fatalf("--tools = %q", got)
	}
	disallowed := "," + argValue(args, "--disallowed-tools") + ","
	for _, tool := range []string{"web_search", "web_fetch", "x_search"} {
		if strings.Contains(disallowed, ","+tool+",") {
			t.Fatalf("native search tool %q remained disallowed: %s", tool, disallowed)
		}
	}
	if slices.Contains(args, "--disable-web-search") {
		t.Fatalf("native search args unexpectedly disable web search: %q", args)
	}
}

func TestGrokBinProviderBuildArgsUsesPrivatePromptFileAndNeutralCWD(t *testing.T) {
	home := t.TempDir()
	p := NewGrokBinProvider("grok-4.5-high", nil)
	p.grokHome = home
	p.sessionID = "grok-session-1"
	promptPath := filepath.Join(home, "prompt.json")
	args, effort, err := p.buildArgs(Request{
		Messages: []Message{SystemText("system instruction"), UserText("private user prompt")},
	}, promptPath)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	if effort != "high" {
		t.Fatalf("effort = %q, want high", effort)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--prompt-file " + promptPath,
		"--output-format streaming-json",
		"--always-approve",
		"--disallowed-tools " + grokDisallowedNativeTools,
		"--max-turns 30",
		"--reasoning-effort high",
		"--resume grok-session-1",
		"--system-prompt-override system instruction",
		"--no-memory",
		"--no-subagents",
		"--no-plan",
		"--disable-web-search",
		"--no-auto-update",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "private user prompt") {
		t.Fatalf("private prompt leaked into argv: %q", joined)
	}
	cwd := argValue(args, "--cwd")
	if cwd != filepath.Join(home, "cwd") {
		t.Fatalf("--cwd = %q, want neutral dir inside GROK_HOME", cwd)
	}
	actualCWD, err := os.Getwd()
	if err == nil && cwd == actualCWD {
		t.Fatalf("--cwd unexpectedly points at project working directory %q", actualCWD)
	}
}

func TestGrokBinProviderRequestEffortOverridesModelSuffix(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5-low", nil)
	p.grokHome = t.TempDir()
	args, effort, err := p.buildArgs(Request{ReasoningEffort: "xhigh"}, filepath.Join(p.grokHome, "prompt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if effort != "xhigh" || argValue(args, "--reasoning-effort") != "xhigh" {
		t.Fatalf("effort = %q args=%v, want xhigh", effort, args)
	}
	if got := argValue(args, "-m"); got != "grok-4.5" {
		t.Fatalf("-m = %q, want grok-4.5", got)
	}
}

func TestGrokBinProviderBuildCommandEnvIsolation(t *testing.T) {
	t.Setenv("GROK_HOME", "/inherited/home")
	t.Setenv("GROK_AUTH_PATH", "/inherited/auth.json")
	t.Setenv("GROK_DISABLE_AUTOUPDATER", "0")
	t.Setenv("XAI_API_KEY", "inherited-secret")
	p := NewGrokBinProvider("grok-4.5", map[string]string{
		"GROK_HOME":         "/configured/home",
		"GROK_AUTH_PATH":    "/configured/auth.json",
		"XAI_API_KEY":       "configured-secret",
		"GROK_EXTRA_OPTION": "yes",
	})
	p.grokHome = t.TempDir()

	env := envSliceMap(p.buildCommandEnv())
	if env["GROK_HOME"] != p.grokHome {
		t.Fatalf("GROK_HOME = %q, want %q", env["GROK_HOME"], p.grokHome)
	}
	if env["GROK_DISABLE_AUTOUPDATER"] != "1" {
		t.Fatalf("GROK_DISABLE_AUTOUPDATER = %q, want 1", env["GROK_DISABLE_AUTOUPDATER"])
	}
	if env["GROK_AUTH_PATH"] != "/configured/auth.json" {
		t.Fatalf("GROK_AUTH_PATH = %q, want provider override", env["GROK_AUTH_PATH"])
	}
	if _, ok := env["XAI_API_KEY"]; ok {
		t.Fatal("XAI_API_KEY should be cleared when OAuth is preferred")
	}
	if env["GROK_EXTRA_OPTION"] != "yes" {
		t.Fatalf("GROK_EXTRA_OPTION = %q", env["GROK_EXTRA_OPTION"])
	}
}

func TestGrokBinProviderBuildCommandEnvDefaultsAuthPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := NewGrokBinProvider("", nil)
	p.grokHome = t.TempDir()
	env := envSliceMap(p.buildCommandEnv())
	want := filepath.Join(os.Getenv("HOME"), ".grok", "auth.json")
	if env["GROK_AUTH_PATH"] != want {
		t.Fatalf("GROK_AUTH_PATH = %q, want %q", env["GROK_AUTH_PATH"], want)
	}
}

func TestGrokBinProviderResumeMessageBoundaries(t *testing.T) {
	p := NewGrokBinProvider("", nil)
	p.sessionID = "session-1"
	p.messagesSent = 2

	messages := []Message{UserText("first"), AssistantText("answer")}
	if _, err := p.messagesForRequest(Request{Messages: messages}); err == nil || !strings.Contains(err.Error(), "no new messages") {
		t.Fatalf("equal resume boundary error = %v", err)
	}
	if p.sessionID != "session-1" || p.messagesSent != 2 {
		t.Fatalf("equal boundary mutated state to (%q, %d)", p.sessionID, p.messagesSent)
	}

	p.messagesSent = 3
	got, err := p.messagesForRequest(Request{Messages: messages})
	if err != nil {
		t.Fatalf("truncated resume boundary: %v", err)
	}
	if len(got) != len(messages) || p.sessionID != "" || p.messagesSent != 0 {
		t.Fatalf("truncated boundary = %d messages, state (%q, %d)", len(got), p.sessionID, p.messagesSent)
	}
}

func TestGrokResumeMessagesKeepsDeferredInterjections(t *testing.T) {
	messages := []Message{
		AssistantText("already in Grok session"),
		ToolResultMessage("call-1", "tool", "already consumed", nil),
		UserText("deferred interjection"),
		UserText("new user turn"),
	}
	got := grokResumeMessages(messages)
	if len(got) != 2 || MessageText(got[0]) != "deferred interjection" || MessageText(got[1]) != "new user turn" {
		t.Fatalf("grokResumeMessages = %+v", got)
	}
}

func TestGrokRecoveryMessagesBoundsReplayAndKeepsLatestUser(t *testing.T) {
	messages := []Message{
		SystemText("system prompt remains session metadata"),
		UserText(strings.Repeat("old user ", 4000)),
		AssistantText(strings.Repeat("old assistant ", 4000)),
		ToolResultMessage("call-1", "read_file", strings.Repeat("huge tool output", 10000), nil),
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "current developer context"}}},
		UserText("latest request: launch both reviewers now"),
	}

	got := grokRecoveryMessages(messages)
	if len(got) == 0 || got[len(got)-1].Role != RoleUser || MessageText(got[len(got)-1]) != "latest request: launch both reviewers now" {
		t.Fatalf("recovery tail = %+v", got)
	}
	if bytes := grokMessagesTextBytes(got); bytes > grokRecoveryTextBudget {
		t.Fatalf("recovery text bytes = %d, want <= %d", bytes, grokRecoveryTextBudget)
	}
	for _, message := range got {
		if message.Role == RoleTool || strings.Contains(MessageText(message), "huge tool output") {
			t.Fatalf("recovery retained tool bulk: %+v", message)
		}
	}
	data, err := buildGrokACPPrompt(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "huge tool output") || !strings.Contains(string(data), "latest request: launch both reviewers now") {
		t.Fatalf("recovery ACP prompt = %s", data)
	}
}

func TestGrokRecoveryMessagesBoundsOversizedLatestUser(t *testing.T) {
	latest := "BEGIN " + strings.Repeat("界", 10_000) + " END"
	got := grokRecoveryMessages([]Message{AssistantText("prior"), UserText(latest)})
	if len(got) < 2 || got[len(got)-1].Role != RoleUser {
		t.Fatalf("recovery messages = %+v", got)
	}
	text := MessageText(got[len(got)-1])
	if len(text) > grokRecoveryLatestUserLimit || !strings.Contains(text, "BEGIN") || !strings.Contains(text, "END") || strings.ToValidUTF8(text, "") != text {
		t.Fatalf("bounded latest user bytes=%d text=%q", len(text), text)
	}
	if !strings.Contains(MessageText(got[len(got)-2]), "Some older turns and tool results were omitted") {
		t.Fatalf("recovery context note missing: %+v", got)
	}
}

func TestGrokRecoveryMessagesWithoutUserAddsBoundedRecoveryPrompt(t *testing.T) {
	messages := []Message{AssistantText(strings.Repeat("answer only", 3000))}
	got := grokRecoveryMessages(messages)
	if len(got) < 2 || got[len(got)-1].Role != RoleUser || MessageText(got[len(got)-1]) != "Continue from the condensed recovery context." {
		t.Fatalf("recovery messages = %+v", got)
	}
	if bytes := grokMessagesTextBytes(got); bytes > grokRecoveryTextBudget {
		t.Fatalf("recovery text bytes = %d, want <= %d", bytes, grokRecoveryTextBudget)
	}
}

func TestGrokBinProviderBuildACPPromptWithImage(t *testing.T) {
	image := base64.StdEncoding.EncodeToString([]byte("png bytes"))
	data, err := buildGrokACPPrompt([]Message{{
		Role: RoleUser,
		Parts: []Part{
			{Type: PartText, Text: "describe this"},
			{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/png", Base64: image}},
		},
	}})
	if err != nil {
		t.Fatalf("buildGrokACPPrompt: %v", err)
	}
	var prompt grokACPPrompt
	if err := json.Unmarshal(data, &prompt); err != nil {
		t.Fatalf("unmarshal ACP prompt: %v", err)
	}
	if prompt.Type != "acp" || len(prompt.Content) != 2 {
		t.Fatalf("prompt = %+v, want acp with text and image", prompt)
	}
	if prompt.Content[0].Type != "text" || !strings.Contains(prompt.Content[0].Text, "describe this") {
		t.Fatalf("text block = %+v", prompt.Content[0])
	}
	if prompt.Content[1].Type != "image" || prompt.Content[1].Data != image || prompt.Content[1].MimeType != "image/png" {
		t.Fatalf("image block = %+v", prompt.Content[1])
	}
}

func TestGrokBinCommandErrorDoesNotExposePromptContent(t *testing.T) {
	p := NewGrokBinProvider("", nil)
	p.grokHome = t.TempDir()
	prompt := []byte(`{"type":"acp","content":[{"type":"text","text":"very private prompt"}]}`)
	systemPrompt := "very private system prompt"
	args := []string{
		"--prompt-file", filepath.Join(p.grokHome, "prompt.json"),
		"--system-prompt-override", systemPrompt,
	}
	err := p.newGrokCommandError(io.ErrUnexpectedEOF, 1, args, "", prompt, "", false,
		[]string{"stdout echoed " + systemPrompt}, []string{"stderr echoed " + systemPrompt})
	diagnostics := strings.Join([]string{
		err.Error(),
		err.CommandLine,
		strings.Join(err.Args, " "),
		err.StdoutTail,
		err.StderrTail,
		err.Stdin,
	}, "\n")
	for _, private := range []string{"very private prompt", systemPrompt} {
		if strings.Contains(diagnostics, private) {
			t.Fatalf("command diagnostics leaked %q: %+v", private, err)
		}
	}
	if got := argValue(err.Args, "--system-prompt-override"); got != "<redacted>" {
		t.Fatalf("redacted system prompt arg = %q", got)
	}
	fields := err.DebugFields()
	if fields["prompt_len"] != len(prompt) || fields["prompt_sha256"] == "" {
		t.Fatalf("prompt diagnostics = %+v", fields)
	}
}

func TestGrokBinCommandErrorDistinguishesProcessAndCLICWD(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5", nil)
	p.grokHome = t.TempDir()
	args, _, err := p.buildArgs(Request{}, filepath.Join(p.grokHome, "prompt.json"))
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	processCWD := t.TempDir()

	commandErr := p.newGrokCommandError(io.ErrUnexpectedEOF, 1, args, "", []byte("prompt"), processCWD, false, nil, nil)
	fields := commandErr.DebugFields()
	if got := fields["cwd"]; got != processCWD {
		t.Fatalf("process cwd = %q, want %q", got, processCWD)
	}
	wantCLICWD := filepath.Join(p.grokHome, "cwd")
	if got := fields["cli_cwd"]; got != wantCLICWD {
		t.Fatalf("Grok --cwd = %q, want neutral cwd %q", got, wantCLICWD)
	}
}

func TestGrokBinProviderHandleLine(t *testing.T) {
	p := NewGrokBinProvider("", nil)
	events := make(chan Event, 8)
	send := eventSender{ctx: context.Background(), ch: events}
	state := grokStreamState{}

	lines := []string{
		`{"type":"thought","data":"thinking"}`,
		`{"type":"text","data":"answer"}`,
		`{"type":"future_event","data":"ignored"}`,
		`{"type":"end","stopReason":"EndTurn","sessionId":"session-123"}`,
	}
	for _, line := range lines {
		if err := p.handleGrokLine(line, false, send, false, &state); err != nil {
			t.Fatalf("handleGrokLine(%s): %v", line, err)
		}
	}
	close(events)
	var got []Event
	for event := range events {
		got = append(got, event)
	}
	if len(got) != 2 || got[0].Type != EventReasoningDelta || got[0].Text != "thinking" || got[1].Type != EventTextDelta || got[1].Text != "answer" {
		t.Fatalf("events = %+v", got)
	}
	if state.sessionID != "session-123" || p.sessionID != "" || state.stopReason != "EndTurn" || !state.sawEnd {
		t.Fatalf("provider/session state = provider session %q state %+v", p.sessionID, state)
	}
}

func TestGrokBinProviderHandleLineErrorFields(t *testing.T) {
	for _, line := range []string{
		`{"type":"error","data":"data error"}`,
		`{"type":"error","message":"message error"}`,
	} {
		p := NewGrokBinProvider("", nil)
		err := p.handleGrokLine(line, false, eventSender{}, false, &grokStreamState{})
		if err == nil || !strings.Contains(err.Error(), "error") {
			t.Fatalf("handleGrokLine(%s) error = %v", line, err)
		}
	}
}

func TestGrokBinProviderMaxTurnsIsWarningNotError(t *testing.T) {
	p := NewGrokBinProvider("", nil)
	events := make(chan Event, 1)
	state := grokStreamState{}
	if err := p.handleGrokLine(`{"type":"max_turns_reached"}`, false, eventSender{ctx: context.Background(), ch: events}, false, &state); err != nil {
		t.Fatalf("handleGrokLine: %v", err)
	}
	event := <-events
	if !state.maxTurnsReached || event.Type != EventPhase || !strings.HasPrefix(event.Text, WarningPhasePrefix) {
		t.Fatalf("state=%+v event=%+v", state, event)
	}
}

func applyGrokTestLines(t *testing.T, p *GrokBinProvider, lines ...string) (grokStreamState, []Event) {
	t.Helper()
	events := make(chan Event, len(lines)+1)
	state := grokStreamState{}
	for _, line := range lines {
		if err := p.handleGrokLine(line, false, eventSender{ctx: context.Background(), ch: events}, false, &state); err != nil {
			t.Fatalf("handleGrokLine(%s): %v", line, err)
		}
	}
	close(events)
	var got []Event
	for event := range events {
		got = append(got, event)
	}
	return state, got
}

func TestGrokBinProviderMaxTurnsExitPersistsTurnAndWarns(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5", nil)
	p.grokHome = t.TempDir()
	state, events := applyGrokTestLines(t, p,
		`{"type":"text","data":"partial answer"}`,
		`{"type":"max_turns_reached"}`,
		`{"type":"end","stopReason":"Cancelled","sessionId":"budget-session"}`,
	)
	request := Request{Messages: []Message{UserText("work")}}
	p.commitGrokResult(request, grokCommandResult{sawEnd: state.sawEnd, sessionID: state.sessionID})
	warned := false
	for _, event := range events {
		if event.Type == EventPhase && strings.HasPrefix(event.Text, WarningPhasePrefix) {
			warned = true
		}
	}
	if !warned || p.sessionID != "budget-session" || p.messagesSent != 1 {
		t.Fatalf("warning/state = %t, %q, %d", warned, p.sessionID, p.messagesSent)
	}
}

func TestGrokBinProviderMaxTurnsWithoutEndIsNonfatalAndDoesNotAdvanceState(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5", nil)
	p.grokHome = t.TempDir()
	state, events := applyGrokTestLines(t, p, `{"type":"max_turns_reached"}`)
	p.commitGrokResult(Request{Messages: []Message{UserText("work")}}, grokCommandResult{sawEnd: state.sawEnd, sessionID: state.sessionID})
	warned := false
	for _, event := range events {
		if event.Type == EventPhase && strings.HasPrefix(event.Text, WarningPhasePrefix) {
			warned = true
		}
	}
	if !warned {
		t.Fatal("max-turns warning was not emitted")
	}
	if p.sessionID != "" || p.messagesSent != 0 {
		t.Fatalf("unterminated max-turns stream advanced state to (%q, %d)", p.sessionID, p.messagesSent)
	}
}

func TestGrokBinProviderSuccessfulExitWithoutEndDoesNotAdvanceState(t *testing.T) {
	p := NewGrokBinProvider("grok-4.5", nil)
	p.grokHome = t.TempDir()
	result, err := grokCommandResultFromState(grokStreamState{})
	if err == nil || !strings.Contains(err.Error(), "without an end event") {
		t.Fatalf("stream finalization error = %v", err)
	}
	p.commitGrokResult(Request{Messages: []Message{UserText("work")}}, result)
	if p.sessionID != "" || p.messagesSent != 0 {
		t.Fatalf("truncated stream advanced state to (%q, %d)", p.sessionID, p.messagesSent)
	}
}

func TestRenderGrokConfig(t *testing.T) {
	configText := renderGrokConfig("http://127.0.0.1:1234/mcp", `token"quoted`)
	for _, want := range []string{
		"[compat.claude]",
		"skills = false",
		"[compat.cursor]",
		"[mcp_servers.term-llm]",
		`url = "http://127.0.0.1:1234/mcp"`,
		`headers = { "Authorization" = "Bearer token\"quoted" }`,
	} {
		if !strings.Contains(configText, want) {
			t.Errorf("config missing %q:\n%s", want, configText)
		}
	}
}

func TestGrokBinProviderWritesConfigInsideDurableHome(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("", nil)
	if err := p.ensureGrokHome(); err != nil {
		t.Fatal(err)
	}
	if err := p.writeConfig("http://127.0.0.1:4321/mcp", "secret-token"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.grokHome, "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "http://127.0.0.1:4321/mcp") || !strings.Contains(string(data), "Bearer secret-token") {
		t.Fatalf("config.toml contents:\n%s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config.toml mode = %o, want private", info.Mode().Perm())
	}
}

func TestGrokBinProviderToolsWithoutExecutorStillWritesIsolationConfig(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("grok-4.5", nil)
	if err := p.ensureGrokHome(); err != nil {
		t.Fatalf("ensure Grok home: %v", err)
	}
	if err := p.writeConfig("", ""); err != nil {
		t.Fatalf("write isolation config: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(p.grokHome, "config.toml"))
	if err != nil {
		t.Fatalf("read isolation config: %v", err)
	}
	configText := string(data)
	if !strings.Contains(configText, "[compat.claude]") || !strings.Contains(configText, "[compat.cursor]") {
		t.Fatalf("isolation config missing compatibility guards:\n%s", configText)
	}
	if strings.Contains(configText, "[mcp_servers.term-llm]") {
		t.Fatalf("isolation config unexpectedly exposed MCP bridge:\n%s", configText)
	}
}

func TestGrokBinProviderStateRoundTripAndRejectsEscapes(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("", nil)
	if err := p.ensureGrokHome(); err != nil {
		t.Fatalf("ensureGrokHome: %v", err)
	}
	p.sessionID = "session-1"
	p.messagesSent = 3
	state, ok := p.ExportProviderState()
	if !ok {
		t.Fatal("ExportProviderState returned false")
	}

	restored := NewGrokBinProvider("", nil)
	if err := restored.ImportProviderState(state); err != nil {
		t.Fatalf("ImportProviderState: %v", err)
	}
	if restored.grokHome != p.grokHome || restored.sessionID != "session-1" || restored.messagesSent != 3 {
		t.Fatalf("restored = home %q session %q sent %d", restored.grokHome, restored.sessionID, restored.messagesSent)
	}
	args, _, err := restored.buildArgs(Request{}, filepath.Join(restored.grokHome, "prompt.json"))
	if err != nil {
		t.Fatalf("buildArgs after restore: %v", err)
	}
	if argValue(args, "--resume") != "session-1" {
		t.Fatalf("restored args missing resume session: %v", args)
	}

	base, err := grokBinCacheBase()
	if err != nil {
		t.Fatal(err)
	}
	malicious, _ := json.Marshal(grokBinProviderState{
		GrokHome:     filepath.Join(base, "..", "outside"),
		SessionID:    "attacker",
		MessagesSent: 1,
	})
	if err := restored.ImportProviderState(malicious); err == nil {
		t.Fatal("ImportProviderState accepted path traversal")
	}
}

func TestGrokBinProviderImportMissingHomeResetsResumeState(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("", nil)
	if err := p.ensureGrokHome(); err != nil {
		t.Fatal(err)
	}
	p.sessionID = "stale-session"
	p.messagesSent = 4
	state, ok := p.ExportProviderState()
	if !ok {
		t.Fatal("ExportProviderState returned false")
	}
	missingHome := p.grokHome
	if err := os.RemoveAll(missingHome); err != nil {
		t.Fatal(err)
	}

	restored := NewGrokBinProvider("", nil)
	if err := restored.ImportProviderState(state); err != nil {
		t.Fatalf("ImportProviderState: %v", err)
	}
	if restored.grokHome != missingHome {
		t.Fatalf("restored home = %q, want %q", restored.grokHome, missingHome)
	}
	if restored.sessionID != "" || restored.messagesSent != 0 {
		t.Fatalf("missing home retained stale state (%q, %d)", restored.sessionID, restored.messagesSent)
	}
	if _, ok := restored.ExportProviderState(); ok {
		t.Fatal("missing home recovery exported stale resume state")
	}
	if _, err := os.Stat(filepath.Join(missingHome, "cwd")); err != nil {
		t.Fatalf("missing home layout was not recreated: %v", err)
	}
}

func TestGrokBinProviderImportRejectsSymlinkEscape(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	base, err := grokBinCacheBase()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(base, "00000000-0000-4000-8000-000000000001")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(grokBinProviderState{GrokHome: link, SessionID: "session", MessagesSent: 1})
	if err := NewGrokBinProvider("", nil).ImportProviderState(data); err == nil {
		t.Fatal("ImportProviderState accepted symlink outside cache base")
	}
}

func TestGrokBinProviderGCRemovesOnlyStaleHomes(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("", nil)
	if err := p.ensureGrokHome(); err != nil {
		t.Fatal(err)
	}
	base, err := grokBinCacheBase()
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(base, "00000000-0000-4000-8000-000000000001")
	fresh := filepath.Join(base, "00000000-0000-4000-8000-000000000002")
	unmanaged := filepath.Join(base, "not-a-grok-home")
	for _, home := range []string{stale, fresh, unmanaged} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".last_used"), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-grokHomeMaxAge - time.Hour)
	for _, home := range []string{stale, unmanaged} {
		marker := filepath.Join(home, ".last_used")
		if err := os.Chtimes(marker, old, old); err != nil {
			t.Fatal(err)
		}
	}

	p.gcStaleGrokHomes(p.grokHome)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale home was not removed: %v", err)
	}
	for _, home := range []string{p.grokHome, fresh, unmanaged} {
		if _, err := os.Stat(home); err != nil {
			t.Fatalf("preserved home %q missing: %v", home, err)
		}
	}
}

func TestGrokBinProviderCleanupKeepsDurableHome(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := NewGrokBinProvider("", nil)
	if err := p.ensureGrokHome(); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(p.grokHome, "prompt-test.json")
	if err := os.WriteFile(prompt, []byte(`{"type":"acp","content":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.trackTempFile(prompt)
	p.CleanupMCP()
	p.CleanupMCP()
	if _, err := os.Stat(p.grokHome); err != nil {
		t.Fatalf("durable GROK_HOME was removed: %v", err)
	}
	if _, err := os.Stat(prompt); !os.IsNotExist(err) {
		t.Fatalf("prompt file was not removed: %v", err)
	}
}

func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func envSliceMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func writeFakeGrok(t *testing.T, dir, output string) {
	t.Helper()
	t.Setenv(grokLegacyTransportEnv, "1")
	path := filepath.Join(dir, "grok")
	script := "#!/bin/sh\ncat <<'EOF'\n" + output + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake grok: %v", err)
	}
}
