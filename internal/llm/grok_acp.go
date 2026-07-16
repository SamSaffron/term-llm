package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	runtimedebug "runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/acp"
	"github.com/samsaffron/term-llm/internal/procutil"
)

const (
	grokACPHandshakeTimeout = 30 * time.Second
	grokACPStopTimeout      = 3 * time.Second
	grokACPProfileName      = "term-llm-acp-agent.md"
	grokLegacyTransportEnv  = "TERM_LLM_GROK_LEGACY_STREAMING_JSON"
)

var errGrokACPResumeUnavailable = errors.New("Grok ACP session could not be restored")

type grokACPProcess struct {
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	stdin      io.WriteCloser
	client     *acp.Client
	conn       *acp.Connection
	handler    *grokACPHandler
	waitDone   chan struct{}
	waitErr    error
	stderrDone chan struct{}

	stderrMu   sync.Mutex
	stderrTail []string

	capabilities acp.AgentCapabilities
	sessionID    string
	mcpURL       string
	model        string
	effort       string
	systemPrompt string
	nativeSearch bool
	stopOnce     sync.Once
}

type grokACPNativeCall struct {
	name string
}

type grokACPTurn struct {
	send          eventSender
	replay        bool
	err           error
	toolBarrier   chan struct{}
	reasoningItem int
	nativeSearch  bool
	nativeCalls   map[string]grokACPNativeCall
}

type grokACPHandler struct {
	mu   sync.Mutex
	turn *grokACPTurn
}

func (h *grokACPHandler) beginTurn(send eventSender, replay bool, nativeSearch bool) {
	h.mu.Lock()
	h.turn = &grokACPTurn{
		send:          send,
		replay:        replay,
		toolBarrier:   make(chan struct{}, 16),
		reasoningItem: 1,
		nativeSearch:  nativeSearch,
		nativeCalls:   make(map[string]grokACPNativeCall),
	}
	h.mu.Unlock()
}

func (h *grokACPHandler) endTurn() {
	h.mu.Lock()
	h.turn = nil
	h.mu.Unlock()
}

func (h *grokACPHandler) turnError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.turn == nil {
		return nil
	}
	return h.turn.err
}

func (h *grokACPHandler) HandleNotification(_ context.Context, method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var notification struct {
		Update json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &notification); err != nil {
		h.setTurnError(fmt.Errorf("decode Grok ACP session update: %w", err))
		return
	}
	var update struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Content       json.RawMessage `json:"content"`
		ToolCallID    string          `json:"toolCallId"`
		Title         string          `json:"title"`
		Kind          string          `json:"kind"`
		Status        string          `json:"status"`
		RawInput      json.RawMessage `json:"rawInput"`
		RawOutput     json.RawMessage `json:"rawOutput"`
		Meta          struct {
			Backend bool `json:"backend"`
			Tool    struct {
				Name string `json:"name"`
			} `json:"x.ai/tool"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(notification.Update, &update); err != nil {
		h.setTurnError(fmt.Errorf("decode Grok ACP session update: %w", err))
		return
	}

	h.mu.Lock()
	turn := h.turn
	if turn == nil || turn.replay || turn.err != nil {
		h.mu.Unlock()
		return
	}
	toolName := update.Meta.Tool.Name
	backendSearch := turn.nativeSearch && update.Meta.Backend && (update.Kind == "search" || update.Kind == "fetch")
	if toolName == "" && update.SessionUpdate == "tool_call" && !backendSearch {
		turn.err = fmt.Errorf("Grok ACP tool call omitted stable tool identity")
		h.mu.Unlock()
		return
	}
	if toolName != "" && !grokACPToolAllowed(toolName, turn.nativeSearch) {
		turn.err = fmt.Errorf("Grok ACP restricted profile attempted native tool %q", toolName)
		h.mu.Unlock()
		return
	}
	var events []Event
	switch update.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(update.Content, &content); err != nil {
			turn.err = fmt.Errorf("decode Grok ACP %s content: %w", update.SessionUpdate, err)
			h.mu.Unlock()
			return
		}
		if content.Type != "text" || content.Text == "" {
			h.mu.Unlock()
			return
		}
		if update.SessionUpdate == "agent_message_chunk" {
			events = append(events, Event{Type: EventTextDelta, Text: content.Text})
		} else {
			events = append(events, Event{
				Type:            EventReasoningDelta,
				Text:            content.Text,
				ReasoningKind:   ReasoningKindRaw,
				ReasoningItemID: fmt.Sprintf("grok-acp-thought-%d", turn.reasoningItem),
			})
		}
	case "tool_call":
		if toolName == "use_tool" {
			select {
			case turn.toolBarrier <- struct{}{}:
			default:
			}
		}
		if backendSearch && update.ToolCallID != "" {
			turn.nativeCalls[update.ToolCallID] = grokACPNativeCall{name: grokACPNativeToolName(update.RawInput, update.Title)}
		}
		// Grok starts a new thought item after both hidden search_tool calls and
		// visible use_tool calls. Preserve that boundary so adjacent chunks do
		// not render as "first.Good" when the intervening tool is observational.
		turn.reasoningItem++
		h.mu.Unlock()
		return
	case "tool_call_update":
		call, ok := turn.nativeCalls[update.ToolCallID]
		if !ok || (update.Status != "completed" && update.Status != "failed") {
			h.mu.Unlock()
			return
		}
		delete(turn.nativeCalls, update.ToolCallID)
		name, args := grokACPNativeToolResult(update.RawOutput, call.name)
		info := ExtractToolInfo(ToolCall{Name: name, Arguments: args})
		events = append(events,
			Event{Type: EventToolExecStart, ToolCallID: update.ToolCallID, ToolName: name, ToolInfo: info, ToolArgs: args},
			Event{Type: EventToolExecEnd, ToolCallID: update.ToolCallID, ToolName: name, ToolInfo: info, ToolSuccess: update.Status == "completed"},
		)
	default:
		// ACP tool-call updates are observational. The authenticated term-llm
		// MCP bridge is the only executable EventToolCall source.
		h.mu.Unlock()
		return
	}
	send := turn.send
	h.mu.Unlock()
	for _, event := range events {
		if err := send.Send(event); err != nil {
			h.setTurnError(err)
			return
		}
	}
}

func grokACPNativeToolName(rawInput json.RawMessage, title string) string {
	var input struct {
		Variant string `json:"variant"`
	}
	if err := json.Unmarshal(rawInput, &input); err == nil {
		switch strings.ToLower(input.Variant) {
		case "xsearch":
			return "x_search"
		case "websearch":
			return "web_search"
		case "webfetch":
			return "web_fetch"
		}
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title), ":"))
	name = strings.ToLower(strings.ReplaceAll(name, " ", "_"))
	if name == "" {
		return "native_search"
	}
	return name
}

func grokACPNativeToolResult(rawOutput json.RawMessage, fallbackName string) (string, json.RawMessage) {
	var output struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(rawOutput, &output); err != nil {
		return fallbackName, nil
	}
	name := strings.TrimSpace(output.Name)
	if name == "" {
		name = fallbackName
	}
	input := output.Input
	if len(input) > 0 && input[0] == '"' {
		var encoded string
		if err := json.Unmarshal(input, &encoded); err != nil || !json.Valid([]byte(encoded)) {
			return name, nil
		}
		input = json.RawMessage(encoded)
	}
	if !json.Valid(input) {
		input = nil
	}
	return name, input
}

func grokACPToolAllowed(toolName string, nativeSearch bool) bool {
	switch toolName {
	case "search_tool", "use_tool":
		return true
	case "web_search", "web_fetch", "x_search":
		return nativeSearch
	default:
		return false
	}
}

func (h *grokACPHandler) waitToolBarrier(ctx context.Context, timeout time.Duration) error {
	h.mu.Lock()
	turn := h.turn
	if turn == nil {
		h.mu.Unlock()
		return nil
	}
	barrier := turn.toolBarrier
	h.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-barrier:
		return nil
	case <-timer.C:
		// Older agents may invoke MCP without an observable use_tool marker.
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *grokACPHandler) HandleRequest(_ context.Context, method string, params json.RawMessage) (any, *acp.RPCError) {
	if method == "session/request_permission" {
		// The restricted Grok profile should never need native-tool approval.
		// Cancel unexpected requests rather than granting access outside the
		// term-llm tool registry.
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	return nil, acp.MethodNotFound(method)
}

func (h *grokACPHandler) setTurnError(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.turn != nil && h.turn.err == nil {
		h.turn.err = err
	}
}

// parseGrokACPUsage normalizes Grok's prompt-completion metadata. Live probes
// against Grok 0.2.93 covered fresh, resumed, and MCP tool-loop turns: the final
// prompt response aggregates the whole ACP turn, inputTokens includes
// cachedReadTokens, and reasoningTokens is a detail within outputTokens.
func parseGrokACPUsage(meta json.RawMessage) (*Usage, error) {
	if len(meta) == 0 || string(meta) == "null" {
		return nil, nil
	}
	var value struct {
		InputTokens      *int `json:"inputTokens"`
		OutputTokens     *int `json:"outputTokens"`
		CachedReadTokens int  `json:"cachedReadTokens"`
		ReasoningTokens  int  `json:"reasoningTokens"`
		TotalTokens      int  `json:"totalTokens"`
	}
	if err := json.Unmarshal(meta, &value); err != nil {
		return nil, fmt.Errorf("decode Grok ACP usage: %w", err)
	}
	if value.InputTokens == nil || value.OutputTokens == nil {
		return nil, nil
	}
	if *value.InputTokens < 0 || *value.OutputTokens < 0 || value.CachedReadTokens < 0 || value.ReasoningTokens < 0 || value.TotalTokens < 0 {
		return nil, fmt.Errorf("decode Grok ACP usage: negative token count")
	}
	if value.CachedReadTokens > *value.InputTokens {
		return nil, fmt.Errorf("decode Grok ACP usage: cached input exceeds raw input")
	}
	return &Usage{
		InputTokens:            *value.InputTokens - value.CachedReadTokens,
		OutputTokens:           *value.OutputTokens,
		CachedInputTokens:      value.CachedReadTokens,
		ProviderRawInputTokens: *value.InputTokens,
		ProviderTotalTokens:    value.TotalTokens,
		ReasoningTokens:        value.ReasoningTokens,
	}, nil
}

func (p *GrokBinProvider) buildACPArgs(req Request, profilePath string) ([]string, string, error) {
	if strings.TrimSpace(p.grokHome) == "" {
		return nil, "", fmt.Errorf("grok-bin GROK_HOME is not initialized")
	}
	neutralCWD := filepath.Join(p.grokHome, "cwd")
	if err := os.MkdirAll(neutralCWD, 0o700); err != nil {
		return nil, "", fmt.Errorf("create grok-bin neutral cwd: %w", err)
	}
	toolAllowlist := "search_tool,use_tool"
	if req.Search {
		toolAllowlist = grokNativeSearchToolAllowlist
	}
	// Keep root flags separate from agent subcommand flags. Grok rejects root
	// options placed after "agent", exiting before the ACP handshake.
	rootArgs := []string{
		"--no-auto-update",
		"--max-turns", fmt.Sprintf("%d", grokMaxTurns),
		"--cwd", neutralCWD,
		"--no-memory",
		"--no-subagents",
		"--no-plan",
		"--tools", toolAllowlist,
		"--disallowed-tools", grokDisallowedTools(req.Search),
	}
	if !req.Search {
		rootArgs = append(rootArgs, "--disable-web-search")
	}
	args := append(rootArgs,
		"agent",
		"--agent-profile", profilePath,
		"--no-leader",
	)
	model, effort := p.grokACPModelEffort(req)
	if model != "" {
		args = append(args, "-m", model)
	}
	if effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	args = append(args, "--always-approve", "stdio")
	return args, effort, nil
}

func (p *GrokBinProvider) grokACPModelEffort(req Request) (string, string) {
	model := chooseModel(req.Model, p.model)
	model, requestModelEffort := parseGrokEffort(model)
	effort := p.effort
	if requestModelEffort != "" {
		effort = requestModelEffort
	}
	if requested := strings.TrimSpace(req.ReasoningEffort); requested != "" {
		effort = requested
	}
	return model, effort
}

func (p *GrokBinProvider) writeACPAgentProfile(nativeSearch bool) (string, error) {
	if strings.TrimSpace(p.grokHome) == "" {
		return "", fmt.Errorf("write grok-bin ACP profile: GROK_HOME is not initialized")
	}
	tools := "  - search_tool\n  - use_tool"
	instructions := "Use only tools supplied through the term-llm MCP server. Never use native filesystem, terminal, web, memory, planning, image, task, or subagent tools."
	if nativeSearch {
		tools += "\n  - web_search\n  - web_fetch\n  - x_search"
		instructions = "Use term-llm MCP tools for all local actions. Native web_search, web_fetch, and x_search are allowed only for read-only web and X research because search is enabled for this request. Never use native filesystem, terminal, memory, planning, image, task, or subagent tools."
	}
	profile := fmt.Sprintf(`---
name: term-llm
description: Restricted transport agent using term-llm MCP tools and optional native search
prompt_mode: full
model: inherit
permission_mode: default
agents_md: false
tools:
%s
---

%s
`, tools, instructions)
	path := filepath.Join(p.grokHome, grokACPProfileName)
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		return "", fmt.Errorf("write grok-bin ACP profile: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("chmod grok-bin ACP profile: %w", err)
	}
	return path, nil
}

func grokACPSessionMeta(systemPrompt string) (json.RawMessage, error) {
	if systemPrompt == "" {
		return nil, nil
	}
	meta, err := json.Marshal(map[string]any{"systemPromptOverride": systemPrompt})
	if err != nil {
		return nil, fmt.Errorf("encode Grok ACP session metadata: %w", err)
	}
	return meta, nil
}

func grokACPMCPServer(url, token string) acp.MCPServer {
	return acp.MCPServer{
		Type: "http",
		Name: "term-llm",
		URL:  url,
		Headers: []acp.Header{{
			Name:  "Authorization",
			Value: "Bearer " + token,
		}},
	}
}

func (p *GrokBinProvider) runGrokACP(ctx context.Context, req Request, messages []Message, debug bool, send eventSender, exposeToolBridge bool) (grokCommandResult, error) {
	promptData, err := buildGrokACPPrompt(messages)
	if err != nil {
		return grokCommandResult{}, err
	}
	var prompt grokACPPrompt
	if err := json.Unmarshal(promptData, &prompt); err != nil {
		return grokCommandResult{}, fmt.Errorf("decode grok-bin ACP prompt: %w", err)
	}
	blocks := make([]acp.ContentBlock, 0, len(prompt.Content))
	for _, block := range prompt.Content {
		blocks = append(blocks, acp.ContentBlock{Type: block.Type, Text: block.Text, Data: block.Data, MimeType: block.MimeType})
	}

	process, temporary, err := p.ensureGrokACPProcess(ctx, req, debug)
	if errors.Is(err, errGrokACPResumeUnavailable) && !req.Ephemeral {
		p.ResetConversation()
		return p.runGrokACP(ctx, req, req.Messages, debug, send, exposeToolBridge)
	}
	if err != nil {
		return grokCommandResult{}, err
	}
	if temporary {
		defer p.stopGrokACPProcess(process)
	}

	bridge := &cliTurnBridge{toolReqCh: make(chan cliToolRequest, 64), done: make(chan struct{})}
	var bridgeDoneOnce sync.Once
	stopBridge := func() { bridgeDoneOnce.Do(func() { close(bridge.done) }) }
	if exposeToolBridge {
		p.cliToolBridgeState.activate(bridge, send.ch)
	}
	defer func() {
		if exposeToolBridge {
			p.cliToolBridgeState.deactivate(bridge)
		}
		stopBridge()
	}()

	process.handler.beginTurn(send, false, process.nativeSearch)
	defer process.handler.endTurn()

	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = process.client.CancelSession(cancelCtx, process.sessionID)
			cancel()
		case <-cancelDone:
		}
	}()

	promptResultCh := make(chan struct {
		response acp.PromptResponse
		err      error
	}, 1)
	go func() {
		response, promptErr := process.client.Prompt(ctx, acp.PromptRequest{SessionID: process.sessionID, Prompt: blocks})
		promptResultCh <- struct {
			response acp.PromptResponse
			err      error
		}{response: response, err: promptErr}
	}()

	var promptResult struct {
		response acp.PromptResponse
		err      error
	}
	for {
		select {
		case promptResult = <-promptResultCh:
			close(cancelDone)
			goto promptComplete
		case toolRequest := <-bridge.toolReqCh:
			if err := process.handler.waitToolBarrier(ctx, grokToolDrainGrace); err != nil {
				toolRequest.ack <- err
				continue
			}
			handleCLIToolRequest(toolRequest, send)
		case <-ctx.Done():
			cancelTimer := time.NewTimer(2 * time.Second)
			for {
				select {
				case promptResult = <-promptResultCh:
					if !cancelTimer.Stop() {
						<-cancelTimer.C
					}
					close(cancelDone)
					goto promptComplete
				case toolRequest := <-bridge.toolReqCh:
					toolRequest.ack <- ctx.Err()
				case <-cancelTimer.C:
					promptResult.err = ctx.Err()
					close(cancelDone)
					goto promptComplete
				}
			}
		}
	}

promptComplete:
	if promptResult.err != nil {
		if !temporary {
			p.discardGrokACPProcess(process)
		}
		return grokCommandResult{}, p.classifyGrokACPError(promptResult.err, process)
	}
	if err := process.handler.turnError(); err != nil {
		if !temporary {
			p.discardGrokACPProcess(process)
		}
		return grokCommandResult{}, err
	}
	usage, usageErr := parseGrokACPUsage(promptResult.response.Meta)
	if usageErr != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[grok-bin] ignoring invalid ACP usage metadata: %v\n", usageErr)
		}
		usage = nil
	}
	if usage != nil {
		if err := send.Send(Event{Type: EventUsage, Use: usage}); err != nil {
			return grokCommandResult{}, err
		}
	}
	switch promptResult.response.StopReason {
	case "max_tokens":
		if err := send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + "Grok reached its token limit; partial output was preserved."}); err != nil {
			return grokCommandResult{}, err
		}
	case "max_turn_requests":
		if err := send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + fmt.Sprintf("Grok CLI reached its %d-turn safety budget. Tool effects and output from this turn were preserved; send a follow-up to continue.", grokMaxTurns)}); err != nil {
			return grokCommandResult{}, err
		}
	case "cancelled":
		if ctx.Err() != nil {
			return grokCommandResult{}, ctx.Err()
		}
		return grokCommandResult{}, nil
	}
	return grokCommandResult{sawEnd: true, sessionID: process.sessionID}, nil
}

func (p *GrokBinProvider) ensureGrokACPProcess(ctx context.Context, req Request, debug bool) (*grokACPProcess, bool, error) {
	if req.Ephemeral {
		// Do not run two Grok processes against the same durable GROK_HOME.
		// Stop the idle conversation process; the next normal turn will restore
		// its ACP session through session/load or session/resume.
		p.acpMu.Lock()
		if p.acpProcess != nil {
			p.stopGrokACPProcess(p.acpProcess)
			p.acpProcess = nil
		}
		p.acpMu.Unlock()
		process, err := p.startGrokACPProcess(ctx, req, debug, "")
		return process, true, err
	}

	p.acpMu.Lock()
	defer p.acpMu.Unlock()
	requestedModel, requestedEffort := p.grokACPModelEffort(req)
	requestedSystemPrompt := extractSystemPrompt(req.Messages)
	if p.acpProcess != nil {
		select {
		case <-p.acpProcess.conn.Done():
			p.stopGrokACPProcess(p.acpProcess)
			p.acpProcess = nil
		default:
			if p.acpProcess.mcpURL == p.mcpURL && p.acpProcess.model == requestedModel && p.acpProcess.effort == requestedEffort && p.acpProcess.systemPrompt == requestedSystemPrompt && p.acpProcess.nativeSearch == req.Search {
				return p.acpProcess, false, nil
			}
			p.stopGrokACPProcess(p.acpProcess)
			p.acpProcess = nil
		}
	}
	process, err := p.startGrokACPProcess(ctx, req, debug, p.sessionID)
	if err != nil {
		return nil, false, err
	}
	p.acpProcess = process
	return process, false, nil
}

func (p *GrokBinProvider) startGrokACPProcess(ctx context.Context, req Request, debug bool, resumeID string) (_ *grokACPProcess, returnErr error) {
	profilePath, err := p.writeACPAgentProfile(req.Search)
	if err != nil {
		return nil, err
	}
	args, effort, err := p.buildACPArgs(req, profilePath)
	if err != nil {
		return nil, err
	}
	model, _ := p.grokACPModelEffort(req)
	systemPrompt := extractSystemPrompt(req.Messages)
	if debug {
		fmt.Fprintf(os.Stderr, "[grok-bin] starting ACP: grok %s\n", shellJoin(redactedGrokArgs(args)))
	}
	processCtx, cancel := context.WithCancel(context.Background())
	cmd, err := newCLICommand(processCtx, "grok", args, req.WorkingDir)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prepare grok ACP command: %w", err)
	}
	cmd.WaitDelay = grokCommandWaitDelay
	cmd.Env = p.buildCommandEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("get grok ACP stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, fmt.Errorf("get grok ACP stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, fmt.Errorf("get grok ACP stderr: %w", err)
	}
	cleanup, err := procutil.PrepareCommand(cmd)
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, fmt.Errorf("prepare grok ACP command: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		cleanup()
		return nil, fmt.Errorf("start grok ACP: %w", err)
	}

	handler := &grokACPHandler{}
	connection := acp.NewConnection(stdout, stdin, handler, acp.Options{})
	process := &grokACPProcess{
		cmd:          cmd,
		cancel:       cancel,
		stdin:        stdin,
		client:       acp.NewClient(connection),
		conn:         connection,
		handler:      handler,
		waitDone:     make(chan struct{}),
		stderrDone:   make(chan struct{}),
		mcpURL:       p.mcpURL,
		model:        model,
		effort:       effort,
		systemPrompt: systemPrompt,
		nativeSearch: req.Search,
	}
	redactDiagnostic := p.grokACPDiagnosticRedactor(req.Messages, cmd.Env)
	go func() {
		defer close(process.stderrDone)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			line := redactDiagnostic(scanner.Text())
			if debug {
				fmt.Fprintf(os.Stderr, "[grok stderr] %s\n", line)
			}
			recordCLITailLine(&process.stderrMu, &process.stderrTail, line, grokStderrTailMaxLines)
		}
	}()
	go func() {
		process.waitErr = cmd.Wait()
		<-process.stderrDone
		cleanup()
		close(process.waitDone)
	}()
	defer func() {
		if returnErr != nil {
			p.stopGrokACPProcess(process)
		}
	}()

	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, grokACPHandshakeTimeout)
	defer handshakeCancel()
	initialize, err := process.client.Initialize(handshakeCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersion1,
		ClientCapabilities: acp.ClientCapabilities{
			FileSystem: acp.FileSystemCapabilities{},
			Terminal:   false,
		},
		ClientInfo: &acp.Implementation{Name: "term-llm", Title: "term-llm", Version: termLLMACPVersion()},
	})
	if err != nil {
		return nil, p.classifyGrokACPError(fmt.Errorf("initialize Grok ACP: %w", err), process)
	}
	if initialize.ProtocolVersion != acp.ProtocolVersion1 {
		return nil, fmt.Errorf("Grok ACP negotiated unsupported protocol version %d", initialize.ProtocolVersion)
	}
	process.capabilities = initialize.AgentCapabilities
	methodID := p.selectGrokACPAuth(initialize.AuthMethods)
	if methodID == "" {
		return nil, &UserFacingProviderError{Summary: "Grok CLI is not logged in", Detail: "Run `grok login` and try again."}
	}
	authMeta, _ := json.Marshal(map[string]any{"headless": true})
	if err := process.client.Authenticate(handshakeCtx, acp.AuthenticateRequest{MethodID: methodID, Meta: authMeta}); err != nil {
		return nil, p.classifyGrokACPError(fmt.Errorf("authenticate Grok ACP: %w", err), process)
	}

	mcpServers := []acp.MCPServer{}
	if p.mcpURL != "" {
		if !initialize.AgentCapabilities.MCPCapabilities.HTTP {
			return nil, fmt.Errorf("Grok ACP does not support HTTP MCP required for term-llm tools")
		}
		mcpServers = append(mcpServers, grokACPMCPServer(p.mcpURL, p.mcpToken))
	}
	neutralCWD := filepath.Join(p.grokHome, "cwd")
	meta, err := grokACPSessionMeta(systemPrompt)
	if err != nil {
		return nil, err
	}

	if resumeID != "" && initialize.AgentCapabilities.SessionCapabilities.SupportsResume() {
		process.handler.beginTurn(eventSender{}, true, process.nativeSearch)
		_, err = process.client.ResumeSession(handshakeCtx, acp.ResumeSessionRequest{SessionID: resumeID, CWD: neutralCWD, MCPServers: mcpServers, Meta: meta})
		process.handler.endTurn()
		if err == nil {
			process.sessionID = resumeID
			return process, nil
		}
	}
	if resumeID != "" && initialize.AgentCapabilities.LoadSession {
		process.handler.beginTurn(eventSender{}, true, process.nativeSearch)
		_, err = process.client.LoadSession(handshakeCtx, acp.LoadSessionRequest{SessionID: resumeID, CWD: neutralCWD, MCPServers: mcpServers, Meta: meta})
		process.handler.endTurn()
		if err == nil {
			process.sessionID = resumeID
			return process, nil
		}
	}
	if resumeID != "" {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if handshakeCtx.Err() != nil {
			return nil, fmt.Errorf("restore Grok ACP session: %w", handshakeCtx.Err())
		}
		return nil, errGrokACPResumeUnavailable
	}
	newSession, err := process.client.NewSession(handshakeCtx, acp.NewSessionRequest{CWD: neutralCWD, MCPServers: mcpServers, Meta: meta})
	if err != nil {
		return nil, p.classifyGrokACPError(fmt.Errorf("create Grok ACP session: %w", err), process)
	}
	if strings.TrimSpace(newSession.SessionID) == "" {
		return nil, fmt.Errorf("create Grok ACP session: empty session ID")
	}
	process.sessionID = strings.TrimSpace(newSession.SessionID)
	return process, nil
}

func termLLMACPVersion() string {
	if info, ok := runtimedebug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func (p *GrokBinProvider) selectGrokACPAuth(methods []acp.AuthMethod) string {
	has := make(map[string]bool, len(methods))
	for _, method := range methods {
		has[method.ID] = true
	}
	if p.preferOAuth && has["cached_token"] {
		return "cached_token"
	}
	if !p.preferOAuth && has["xai.api_key"] {
		return "xai.api_key"
	}
	if has["cached_token"] {
		return "cached_token"
	}
	if has["xai.api_key"] {
		return "xai.api_key"
	}
	return ""
}

func (p *GrokBinProvider) grokACPDiagnosticRedactor(messages []Message, env []string) func(string) string {
	secrets := make([]string, 0, len(messages)+4)
	for _, message := range messages {
		for _, part := range message.Parts {
			secrets = appendGrokACPDiagnosticSecret(secrets, part.Text)
			secrets = appendGrokACPDiagnosticSecret(secrets, part.ReasoningContent)
			if part.ImageData != nil {
				secrets = appendGrokACPDiagnosticSecret(secrets, part.ImageData.Base64)
			}
			if part.FileData != nil {
				secrets = appendGrokACPDiagnosticSecret(secrets, part.FileData.Base64)
			}
			if part.ToolCall != nil {
				secrets = appendGrokACPDiagnosticSecret(secrets, string(part.ToolCall.Arguments))
			}
			if part.ToolResult != nil {
				secrets = appendGrokACPDiagnosticSecret(secrets, part.ToolResult.Content)
				for _, content := range part.ToolResult.ContentParts {
					secrets = appendGrokACPDiagnosticSecret(secrets, content.Text)
					if content.ImageData != nil {
						secrets = appendGrokACPDiagnosticSecret(secrets, content.ImageData.Base64)
					}
				}
			}
		}
	}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || redactEnvValue(key, value) != "[redacted]" {
			continue
		}
		secrets = appendGrokACPDiagnosticSecret(secrets, value)
	}
	secrets = appendGrokACPDiagnosticSecret(secrets, p.mcpToken)
	return func(text string) string {
		for _, secret := range secrets {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
		return text
	}
}

func appendGrokACPDiagnosticSecret(secrets []string, secret string) []string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return secrets
	}
	return append(secrets, secret)
}

func (p *GrokBinProvider) classifyGrokACPError(err error, process *grokACPProcess) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	var rpcErr *acp.RPCError
	if errors.As(err, &rpcErr) {
		lower := strings.ToLower(rpcErr.Message)
		if strings.Contains(lower, "auth") || strings.Contains(lower, "login") {
			return &UserFacingProviderError{Summary: "Grok CLI is not logged in", Detail: rpcErr.Message, Cause: err}
		}
	}
	if process != nil {
		select {
		case <-process.waitDone:
			tail := snapshotCLITail(&process.stderrMu, process.stderrTail)
			return fmt.Errorf("Grok ACP process exited: %w (stderr: %s)", process.waitErr, strings.Join(normalizeCLITail(tail), "\n"))
		default:
		}
	}
	return err
}

func (p *GrokBinProvider) discardGrokACPProcess(process *grokACPProcess) {
	p.acpMu.Lock()
	defer p.acpMu.Unlock()
	if p.acpProcess == process {
		p.acpProcess = nil
	}
	p.stopGrokACPProcess(process)
}

func (p *GrokBinProvider) stopGrokACPProcess(process *grokACPProcess) {
	if process == nil {
		return
	}
	process.stopOnce.Do(func() {
		if process.capabilities.SessionCapabilities.SupportsClose() && process.sessionID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			closeDone := make(chan struct{})
			go func() {
				defer close(closeDone)
				_ = process.client.CloseSession(ctx, acp.CloseSessionRequest{SessionID: process.sessionID})
			}()
			select {
			case <-closeDone:
			case <-ctx.Done():
			}
			cancel()
		}
		_ = process.stdin.Close()
		process.cancel()
		select {
		case <-process.waitDone:
		case <-time.After(grokACPStopTimeout):
			if process.cmd.Process != nil {
				_ = process.cmd.Process.Kill()
			}
			select {
			case <-process.waitDone:
			case <-time.After(time.Second):
			}
		}
	})
}

func requestHasImages(messages []Message) bool {
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == PartImage || (part.Type == PartToolResult && part.ToolResult != nil && toolResultHasImageData(part.ToolResult)) {
				return true
			}
		}
	}
	return false
}
