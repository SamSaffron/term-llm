package clibin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/acp"
	"github.com/samsaffron/term-llm/internal/cliwire"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/procutil"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

const (
	// vibe-acp is a Python program that starts its harness and loads the model
	// catalog before answering session/new, so allow a slower handshake.
	vibeACPHandshakeTimeout    = 90 * time.Second
	vibeACPCancelSettleTimeout = 5 * time.Second
	vibeACPStopTimeout         = 3 * time.Second
	vibeACPCloseTimeout        = time.Second
	vibeToolBarrierGraceEnv    = "TERM_LLM_VIBE_TOOL_BARRIER_GRACE_MS"
	vibeNotSignedInDetail      = "Run `vibe` on this machine and sign in with your Mistral account. " +
		"vibe-bin reuses OS-keyring sign-ins but does not import Vibe's .env file. " +
		"If Vibe saved your sign-in there, configure that key explicitly under providers.<name>.env.MISTRAL_API_KEY; billing and quota follow that credential."
)

var errVibeResumeUnavailable = errors.New("Vibe session could not be restored")

// Vibe announces a term-llm MCP call with a tool_call update before invoking
// the MCP server, so the barrier normally opens before the request arrives.
var vibeToolBarrierGrace = loadCLIToolLineDrainGrace(vibeToolBarrierGraceEnv, 250*time.Millisecond)

type vibeACPProcess struct {
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	stdin      io.WriteCloser
	client     *acp.Client
	conn       *acp.Connection
	handler    *vibeACPHandler
	waitDone   chan struct{}
	waitErr    error
	stderrDone chan struct{}

	stderrMu   sync.Mutex
	stderrTail []string

	capabilities acp.AgentCapabilities
	sessionID    string
	mcpURL       string
	model        string
	wireAudit    *cliwire.Server
	stopOnce     sync.Once
}

// vibeUsageCounters are Vibe's cumulative session token counters. Input
// includes cached reads.
type vibeUsageCounters struct {
	input, output, cached int
}

type vibeACPTurn struct {
	send        eventSender
	err         error
	toolBarrier chan struct{}
}

type vibeACPHandler struct {
	mu        sync.Mutex
	turn      *vibeACPTurn
	toolNames map[string]string
	usage     vibeUsageCounters
	debug     bool
}

func newVibeACPHandler(debug bool) *vibeACPHandler {
	return &vibeACPHandler{toolNames: make(map[string]string), debug: debug}
}

func (h *vibeACPHandler) beginTurn(send eventSender) vibeUsageCounters {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turn = &vibeACPTurn{send: send, toolBarrier: make(chan struct{}, 16)}
	return h.usage
}

func (h *vibeACPHandler) endTurn() {
	h.mu.Lock()
	h.turn = nil
	h.mu.Unlock()
}

func (h *vibeACPHandler) turnError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.turn == nil {
		return nil
	}
	return h.turn.err
}

func (h *vibeACPHandler) setTurnError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.turn != nil && h.turn.err == nil {
		h.turn.err = err
	}
}

func (h *vibeACPHandler) setUsage(usage vibeUsageCounters) {
	h.mu.Lock()
	h.usage = usage
	h.mu.Unlock()
}

func (h *vibeACPHandler) HandleNotification(_ context.Context, method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var notification struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			MessageID     string          `json:"messageId"`
			ToolCallID    string          `json:"toolCallId"`
			Status        string          `json:"status"`
			Content       json.RawMessage `json:"content"`
			Meta          struct {
				ToolName         string `json:"tool_name"`
				PromptTokens     *int   `json:"promptTokens"`
				CompletionTokens *int   `json:"completionTokens"`
				CachedTokens     int    `json:"cachedTokens"`
			} `json:"_meta"`
		} `json:"update"`
	}
	if err := json.Unmarshal(params, &notification); err != nil {
		h.setTurnError(fmt.Errorf("decode Vibe ACP session update: %w", err))
		return
	}
	update := notification.Update
	if h.debug && update.SessionUpdate != "agent_message_chunk" && update.SessionUpdate != "agent_thought_chunk" {
		runtimeoutput.Printf("[vibe-bin] update %s tool=%q status=%q id=%q\n", update.SessionUpdate, update.Meta.ToolName, update.Status, update.ToolCallID)
	}

	h.mu.Lock()
	if update.ToolCallID != "" && update.Meta.ToolName != "" {
		h.toolNames[update.ToolCallID] = update.Meta.ToolName
	}
	if update.SessionUpdate == "usage_update" && update.Meta.PromptTokens != nil && update.Meta.CompletionTokens != nil {
		h.usage = vibeUsageCounters{input: *update.Meta.PromptTokens, output: *update.Meta.CompletionTokens, cached: update.Meta.CachedTokens}
	}
	turn := h.turn
	// No active turn also covers session/load, which replays the stored history.
	if turn == nil || turn.err != nil {
		h.mu.Unlock()
		return
	}
	var event core.Event
	switch update.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(update.Content, &content); err != nil {
			turn.err = fmt.Errorf("decode Vibe ACP %s content: %w", update.SessionUpdate, err)
			h.mu.Unlock()
			return
		}
		if content.Type != "text" || content.Text == "" {
			h.mu.Unlock()
			return
		}
		if update.SessionUpdate == "agent_message_chunk" {
			event = core.Event{Type: core.EventTextDelta, Text: content.Text}
		} else {
			event = core.Event{
				Type:            core.EventReasoningDelta,
				Text:            content.Text,
				ReasoningKind:   core.ReasoningKindRaw,
				ReasoningItemID: "vibe-" + update.MessageID,
			}
		}
	case "tool_call":
		if strings.HasPrefix(update.Meta.ToolName, vibeMCPToolPrefix) {
			select {
			case turn.toolBarrier <- struct{}{}:
			default:
			}
		}
		// Tool updates are observational: the authenticated term-llm MCP
		// bridge is the only executable EventToolCall source.
		h.mu.Unlock()
		return
	default:
		h.mu.Unlock()
		return
	}
	send := turn.send
	h.mu.Unlock()
	if err := send.Send(event); err != nil {
		h.setTurnError(err)
	}
}

// HandleRequest approves only term-llm MCP tools. Vibe's own tools are already
// outside enabled_tools; denying here keeps that guarantee if a Vibe version
// or user config routes them through approval instead.
func (h *vibeACPHandler) HandleRequest(_ context.Context, method string, params json.RawMessage) (any, *acp.RPCError) {
	if method != "session/request_permission" {
		return nil, acp.MethodNotFound(method)
	}
	var request struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(params, &request); err == nil {
		h.mu.Lock()
		name := h.toolNames[request.ToolCall.ToolCallID]
		h.mu.Unlock()
		allowed := strings.HasPrefix(name, vibeMCPToolPrefix)
		if h.debug {
			runtimeoutput.Printf("[vibe-bin] permission tool=%q id=%q allowed=%t\n", name, request.ToolCall.ToolCallID, allowed)
		}
		if allowed {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow_once"}}, nil
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
}

func (h *vibeACPHandler) waitToolBarrier(ctx context.Context, timeout time.Duration) error {
	h.mu.Lock()
	turn := h.turn
	h.mu.Unlock()
	if turn == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-turn.toolBarrier:
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// parseVibeUsage decodes the cumulative session usage in a prompt response.
func parseVibeUsage(raw json.RawMessage) (vibeUsageCounters, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return vibeUsageCounters{}, false
	}
	var value struct {
		InputTokens      *int `json:"inputTokens"`
		OutputTokens     *int `json:"outputTokens"`
		CachedReadTokens int  `json:"cachedReadTokens"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.InputTokens == nil || value.OutputTokens == nil {
		return vibeUsageCounters{}, false
	}
	usage := vibeUsageCounters{input: *value.InputTokens, output: *value.OutputTokens, cached: value.CachedReadTokens}
	if usage.input < 0 || usage.output < 0 || usage.cached < 0 || usage.cached > usage.input {
		return vibeUsageCounters{}, false
	}
	return usage, true
}

// vibeTurnUsage converts cumulative counters into this turn's usage. A counter
// below its baseline means Vibe restarted its accounting, so the cumulative
// value is the turn's own.
func vibeTurnUsage(baseline, final vibeUsageCounters) *core.Usage {
	delta := func(final, base int) int {
		if final < base {
			return final
		}
		return final - base
	}
	input := delta(final.input, baseline.input)
	output := delta(final.output, baseline.output)
	cached := delta(final.cached, baseline.cached)
	if cached > input {
		cached = input
	}
	if input == 0 && output == 0 {
		return nil
	}
	return &core.Usage{
		InputTokens:            input - cached,
		OutputTokens:           output,
		CachedInputTokens:      cached,
		ProviderRawInputTokens: input,
		ProviderTotalTokens:    input + output,
	}
}

func (p *VibeBinProvider) sendNativeInterrupt() {
	p.acpMu.Lock()
	process := p.acpProcess
	p.acpMu.Unlock()
	if process == nil || process.sessionID == "" || !p.acpPromptActive.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p.expectedCancel.Store(true)
	if delivered, err := process.client.CancelSessionWithDelivery(ctx, process.sessionID); err != nil && !delivered {
		p.expectedCancel.Store(false)
		return
	}
	p.clearInlineFlush()
}

type vibeTurnResult struct {
	sessionID string
}

type vibePromptResult struct {
	response acp.PromptResponse
	err      error
}

func (p *VibeBinProvider) runVibeACP(ctx context.Context, req core.Request, messages []core.Message, debug bool, send eventSender, exposeToolBridge bool) (vibeTurnResult, error) {
	process, fresh, temporary, err := p.ensureVibeACPProcess(ctx, req, debug)
	if errors.Is(err, errVibeResumeUnavailable) && !req.Ephemeral {
		p.ResetConversation()
		if err := send.Send(core.Event{Type: core.EventPhase, Text: core.WarningPhasePrefix + "Vibe's session could not be restored; continuing from a condensed excerpt of the conversation."}); err != nil {
			return vibeTurnResult{}, err
		}
		return p.runVibeACP(ctx, req, grokRecoveryMessages(req.Messages), debug, send, exposeToolBridge)
	}
	if err != nil {
		return vibeTurnResult{}, err
	}
	if temporary {
		defer process.stop()
	}
	blocks, err := vibeTurnBlocks(req, messages, fresh, exposeToolBridge)
	if err != nil {
		return vibeTurnResult{}, err
	}

	bridge := &cliTurnBridge{toolReqCh: make(chan cliToolRequest, 64), done: make(chan struct{})}
	if exposeToolBridge {
		p.cliToolBridgeState.activate(bridge, send.ch)
	}
	defer func() {
		if exposeToolBridge {
			p.cliToolBridgeState.deactivate(bridge)
		}
		close(bridge.done)
	}()

	baseline := process.handler.beginTurn(send)
	defer process.handler.endTurn()
	p.expectedCancel.Store(false)
	if debug {
		runtimeoutput.Printf("[vibe-bin] prompting ACP session %s (blocks=%d, fresh=%t)\n", process.sessionID, len(blocks), fresh)
	}
	result := p.awaitVibePrompt(ctx, process, blocks, bridge, send, debug)
	if err := p.checkVibeTurnOutcome(ctx, process, result, temporary, debug); err != nil {
		return vibeTurnResult{}, err
	}
	if err := emitVibeUsage(ctx, process, result.response, baseline, send); err != nil {
		return vibeTurnResult{}, err
	}
	if debug {
		runtimeoutput.Printf("[vibe-bin] ACP prompt completed (stop_reason=%s)\n", result.response.StopReason)
	}
	warning := vibeStopWarning(result.response.StopReason, ctx.Err() == nil && !p.expectedCancel.Load())
	p.expectedCancel.Store(false)
	if warning != "" {
		if err := send.Send(core.Event{Type: core.EventPhase, Text: core.WarningPhasePrefix + warning}); err != nil {
			return vibeTurnResult{}, err
		}
	}
	if req.Ephemeral {
		return vibeTurnResult{}, nil
	}
	return vibeTurnResult{sessionID: process.sessionID}, nil
}

// vibeTurnBlocks builds the prompt. Only a fresh session needs the tool policy
// and system prompt; a resumed one already holds them.
func vibeTurnBlocks(req core.Request, messages []core.Message, fresh, exposeToolBridge bool) ([]acp.ContentBlock, error) {
	if !fresh {
		return vibePromptBlocks(messages, "", "")
	}
	var tools []core.ToolSpec
	if exposeToolBridge {
		tools = req.Tools
	}
	return vibePromptBlocks(messages, vibeToolPolicy(tools), extractSystemPrompt(req.Messages))
}

// awaitVibePrompt sends session/prompt and dispatches term-llm tool calls until
// Vibe answers. A caller cancel becomes session/cancel; if Vibe does not settle
// in time the result carries the context error so the process is discarded.
func (p *VibeBinProvider) awaitVibePrompt(ctx context.Context, process *vibeACPProcess, blocks []acp.ContentBlock, bridge *cliTurnBridge, send eventSender, debug bool) vibePromptResult {
	cancelDone := make(chan struct{})
	defer close(cancelDone)
	promptCtx, cancelPrompt := context.WithCancel(context.Background())
	defer cancelPrompt()
	resultCh := make(chan vibePromptResult, 1)
	go func() {
		var response acp.PromptResponse
		err := process.client.Connection().CallAfterWrite(promptCtx, "session/prompt", acp.PromptRequest{SessionID: process.sessionID, Prompt: blocks}, &response, func() {
			p.acpPromptActive.Store(true)
			// Arm cancellation only once session/prompt is on the wire.
			go func() {
				select {
				case <-ctx.Done():
					cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
					_ = process.client.CancelSession(cancelCtx, process.sessionID)
					cancel()
				case <-cancelDone:
				}
			}()
			if p.inlineFlushRequested() {
				p.sendNativeInterrupt()
			}
		})
		resultCh <- vibePromptResult{response: response, err: err}
	}()
	defer p.acpPromptActive.Store(false)

	for {
		select {
		case result := <-resultCh:
			return result
		case toolRequest := <-bridge.toolReqCh:
			if debug {
				runtimeoutput.Printf("[vibe-bin] MCP tool request %s (%s)\n", toolRequest.name, toolRequest.callID)
			}
			if err := process.handler.waitToolBarrier(ctx, vibeToolBarrierGrace); err != nil {
				toolRequest.ack <- err
				continue
			}
			handleCLIToolRequest(toolRequest, send)
		case <-ctx.Done():
			return awaitVibeCancelSettle(ctx, resultCh, bridge)
		}
	}
}

func awaitVibeCancelSettle(ctx context.Context, resultCh <-chan vibePromptResult, bridge *cliTurnBridge) vibePromptResult {
	settle := time.NewTimer(vibeACPCancelSettleTimeout)
	defer settle.Stop()
	for {
		select {
		case result := <-resultCh:
			return result
		case toolRequest := <-bridge.toolReqCh:
			toolRequest.ack <- ctx.Err()
		case <-settle.C:
			// Vibe did not acknowledge the cancel; its session contents are
			// uncertain, so the caller discards the process.
			return vibePromptResult{err: ctx.Err()}
		}
	}
}

// checkVibeTurnOutcome discards the process after a failed prompt or turn.
func (p *VibeBinProvider) checkVibeTurnOutcome(ctx context.Context, process *vibeACPProcess, result vibePromptResult, temporary, debug bool) error {
	discard := func() {
		if !temporary {
			p.discardVibeACPProcess(process)
		}
	}
	if result.err != nil {
		if debug {
			runtimeoutput.Printf("[vibe-bin] ACP prompt failed: %v\n", result.err)
		}
		discard()
		return p.classifyVibeACPError(result.err, process)
	}
	turnErr := process.handler.turnError()
	if turnErr == nil {
		return nil
	}
	// Late deltas after a caller cancel cannot reach the closed stream; that
	// must not discard a session whose prompt still settled cleanly.
	if ctx.Err() != nil && (errors.Is(turnErr, context.Canceled) || errors.Is(turnErr, context.DeadlineExceeded) || errors.Is(turnErr, errEventStreamClosed)) {
		return nil
	}
	discard()
	return turnErr
}

func emitVibeUsage(ctx context.Context, process *vibeACPProcess, response acp.PromptResponse, baseline vibeUsageCounters, send eventSender) error {
	final, ok := parseVibeUsage(response.Usage)
	if !ok {
		return nil
	}
	process.handler.setUsage(final)
	usage := vibeTurnUsage(baseline, final)
	if usage == nil {
		return nil
	}
	event := core.Event{Type: core.EventUsage, Use: usage}
	if ctx.Err() != nil {
		// Best effort: the turn still commits after the caller cancelled.
		send.TrySend(event)
		return nil
	}
	return send.Send(event)
}

// vibeStopWarning explains non-normal stop reasons. A cancelled turn still
// commits as a boundary because Vibe keeps it in its session; it is only
// worth a warning when term-llm did not request it.
func vibeStopWarning(stopReason string, unexpectedCancel bool) string {
	switch stopReason {
	case "max_tokens":
		return "Vibe reached its output token limit; partial output was preserved."
	case "max_turn_requests":
		return fmt.Sprintf("Vibe reached its %d-step turn limit. Tool effects and output from this turn were preserved; send a follow-up to continue.", vibeMaxTurns)
	case "refusal":
		return "Vibe declined to continue this turn."
	case "cancelled":
		if unexpectedCancel {
			return "Vibe cancelled the turn before producing a complete response."
		}
	}
	return ""
}

// ensureVibeACPProcess returns a process with a live session. fresh reports a
// newly created session, which needs term-llm's system prompt and full
// transcript; temporary processes serve ephemeral requests and must be stopped.
func (p *VibeBinProvider) ensureVibeACPProcess(ctx context.Context, req core.Request, debug bool) (process *vibeACPProcess, fresh, temporary bool, err error) {
	model := strings.TrimSpace(core.ChooseModel(req.Model, p.model))
	p.mcpMu.Lock()
	mcpURL, mcpToken := p.mcpURL, p.mcpToken
	p.mcpMu.Unlock()
	if req.Ephemeral {
		process, err := p.startVibeACPProcess(ctx, debug, model, mcpURL, mcpToken, "")
		return process, true, true, err
	}

	p.acpMu.Lock()
	defer p.acpMu.Unlock()
	if p.acpProcess != nil {
		select {
		case <-p.acpProcess.conn.Done():
		default:
			if p.acpProcess.mcpURL == mcpURL && p.acpProcess.model == model && p.acpProcess.sessionID == p.sessionID {
				return p.acpProcess, false, false, nil
			}
		}
		p.acpProcess.stop()
		p.acpProcess = nil
	}
	process, err = p.startVibeACPProcess(ctx, debug, model, mcpURL, mcpToken, p.sessionID)
	if err != nil {
		return nil, false, false, err
	}
	p.acpProcess = process
	return process, p.sessionID == "", false, nil
}

func (p *VibeBinProvider) startVibeACPProcess(ctx context.Context, debug bool, model, mcpURL, mcpToken, resumeID string) (_ *vibeACPProcess, returnErr error) {
	cwd, err := vibeNeutralCWD()
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(vibeACPBinary); err != nil {
		return nil, &UserFacingProviderError{
			Summary: "Mistral Vibe CLI is not installed",
			Detail:  "vibe-bin needs `vibe-acp` on PATH; install Mistral Vibe (for example `uv tool install mistral-vibe`) and sign in with `vibe`.",
			Cause:   err,
		}
	}
	home, err := vibeRuntimeHome(cwd)
	if err != nil {
		return nil, err
	}
	if debug {
		runtimeoutput.Printf("[vibe-bin] starting %s (model=%q, resume=%q)\n", vibeACPBinary, model, resumeID)
	}
	processCtx, cancel := context.WithCancel(context.Background())
	cmd, err := newCLICommand(processCtx, vibeACPBinary, nil, cwd)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prepare vibe-acp command: %w", err)
	}
	cmd.WaitDelay = time.Second
	auditedEnv, wireAudit, err := startCLIWireAudit("vibe-bin", p.buildVibeCommandEnv(model, home))
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Env = auditedEnv
	fail := func(format string, err error) error {
		cancel()
		stopCLIWireAudit(wireAudit)
		return fmt.Errorf(format, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fail("get vibe-acp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fail("get vibe-acp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fail("get vibe-acp stderr: %w", err)
	}
	cleanup, err := procutil.PrepareCommand(cmd)
	if err != nil {
		_ = stdin.Close()
		return nil, fail("prepare vibe-acp command: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fail("start vibe-acp: %w", err)
	}

	handler := newVibeACPHandler(debug)
	connection := acp.NewConnection(stdout, stdin, handler, acp.Options{})
	process := &vibeACPProcess{
		cmd:        cmd,
		cancel:     cancel,
		stdin:      stdin,
		client:     acp.NewClient(connection),
		conn:       connection,
		handler:    handler,
		waitDone:   make(chan struct{}),
		stderrDone: make(chan struct{}),
		mcpURL:     mcpURL,
		model:      model,
		wireAudit:  wireAudit,
	}
	go func() {
		defer close(process.stderrDone)
		_ = drainCLIDiagnosticLines(stderr, func(line string) {
			if mcpToken != "" {
				line = strings.ReplaceAll(line, mcpToken, "[redacted]")
			}
			if debug {
				runtimeoutput.Printf("[vibe-acp stderr] %s\n", line)
			}
			recordCLITailLine(&process.stderrMu, &process.stderrTail, line, vibeStderrTailLines)
		})
	}()
	go func() {
		process.waitErr = cmd.Wait()
		<-process.stderrDone
		cleanup()
		close(process.waitDone)
	}()
	defer func() {
		if returnErr != nil {
			process.stop()
		}
	}()

	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, vibeACPHandshakeTimeout)
	defer handshakeCancel()
	stopWatchdog := context.AfterFunc(handshakeCtx, func() {
		// ACP writes do not observe their context while blocked in Write.
		_ = process.stdin.Close()
		process.cancel()
	})
	defer stopWatchdog()

	initialize, err := process.client.Initialize(handshakeCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersion1,
		ClientInfo:      &acp.Implementation{Name: "term-llm", Title: "term-llm", Version: termLLMACPVersion()},
	})
	if err != nil {
		return nil, p.classifyVibeACPError(fmt.Errorf("initialize Vibe ACP: %w", err), process)
	}
	if initialize.ProtocolVersion != acp.ProtocolVersion1 {
		return nil, fmt.Errorf("Vibe ACP negotiated unsupported protocol version %d", initialize.ProtocolVersion)
	}
	process.capabilities = initialize.AgentCapabilities

	// Vibe accepts HTTP MCP servers without advertising mcpCapabilities.http.
	mcpServers := []acp.MCPServer{}
	if mcpURL != "" {
		mcpServers = append(mcpServers, acp.MCPServer{
			Type:    "http",
			Name:    vibeMCPServerName,
			URL:     mcpURL,
			Headers: []acp.Header{{Name: "Authorization", Value: "Bearer " + mcpToken}},
		})
	}

	if resumeID != "" {
		if !initialize.AgentCapabilities.LoadSession {
			return nil, errVibeResumeUnavailable
		}
		// Replayed history arrives while no turn is active and is dropped.
		loaded, err := process.client.LoadSession(handshakeCtx, acp.LoadSessionRequest{SessionID: resumeID, CWD: cwd, MCPServers: mcpServers})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if classified := p.classifyVibeACPError(err, process); isVibeNotSignedIn(classified) {
				return nil, classified
			}
			if debug {
				runtimeoutput.Printf("[vibe-bin] session/load %s failed: %v\n", resumeID, err)
			}
			return nil, errVibeResumeUnavailable
		}
		process.sessionID = resumeID
		if err := checkVibeModel(loaded.ConfigOptions, model); err != nil {
			return nil, err
		}
	} else {
		session, err := process.client.NewSession(handshakeCtx, acp.NewSessionRequest{CWD: cwd, MCPServers: mcpServers})
		if err != nil {
			return nil, p.classifyVibeACPError(fmt.Errorf("create Vibe ACP session: %w", err), process)
		}
		process.sessionID = strings.TrimSpace(session.SessionID)
		if process.sessionID == "" {
			return nil, fmt.Errorf("create Vibe ACP session: empty session ID")
		}
		if err := checkVibeModel(session.ConfigOptions, model); err != nil {
			return nil, err
		}
	}
	// Session settings live only in this session; unlike the model and thinking
	// options they are never written to the user's Vibe config.
	if err := process.client.Connection().Call(handshakeCtx, "session/set_config_option", map[string]any{
		"sessionId": process.sessionID, "configId": "max_turns", "value": fmt.Sprint(vibeMaxTurns),
	}, &json.RawMessage{}); err != nil && debug {
		runtimeoutput.Printf("[vibe-bin] could not set max_turns: %v\n", err)
	}
	if !stopWatchdog() {
		return nil, fmt.Errorf("complete Vibe ACP handshake: %w", context.Cause(handshakeCtx))
	}
	return process, nil
}

// checkVibeModel verifies that Vibe actually selected the requested model:
// an unknown VIBE_ACTIVE_MODEL alias would otherwise silently run on another.
func checkVibeModel(raw json.RawMessage, model string) error {
	if model == "" || len(raw) == 0 {
		return nil
	}
	var options []struct {
		ID           string `json:"id"`
		CurrentValue string `json:"currentValue"`
		Options      []struct {
			Value string `json:"value"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil
	}
	for _, option := range options {
		if option.ID != "model" || option.CurrentValue == "" || option.CurrentValue == model {
			continue
		}
		available := make([]string, 0, len(option.Options))
		for _, value := range option.Options {
			available = append(available, value.Value)
		}
		return &UserFacingProviderError{
			Summary: fmt.Sprintf("Vibe does not offer model %q", model),
			Detail:  "Available Vibe models: " + strings.Join(available, ", "),
		}
	}
	return nil
}

func isVibeNotSignedIn(err error) bool {
	var userErr *UserFacingProviderError
	return errors.As(err, &userErr) && userErr.Detail == vibeNotSignedInDetail
}

func (p *VibeBinProvider) classifyVibeACPError(err error, process *vibeACPProcess) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	var rpcErr *acp.RPCError
	if errors.As(err, &rpcErr) {
		lower := strings.ToLower(rpcErr.Message)
		if strings.Contains(lower, "unauthenticated") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "api key") || strings.Contains(lower, "sign in") {
			return &UserFacingProviderError{Summary: "Mistral Vibe is not signed in", Detail: vibeNotSignedInDetail, Cause: err}
		}
	}
	if process != nil {
		select {
		case <-process.waitDone:
			tail := snapshotCLITail(&process.stderrMu, process.stderrTail)
			return fmt.Errorf("vibe-acp exited: %w (stderr: %s)", process.waitErr, strings.Join(normalizeCLITail(tail), "\n"))
		default:
		}
	}
	return err
}

func (p *VibeBinProvider) discardVibeACPProcess(process *vibeACPProcess) {
	p.acpMu.Lock()
	defer p.acpMu.Unlock()
	if p.acpProcess == process {
		p.acpProcess = nil
	}
	process.stop()
}

func (process *vibeACPProcess) stop() {
	if process == nil {
		return
	}
	process.stopOnce.Do(func() {
		if process.capabilities.SessionCapabilities.SupportsClose() && process.sessionID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), vibeACPCloseTimeout)
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
		case <-time.After(vibeACPStopTimeout):
			if process.cmd.Process != nil {
				_ = process.cmd.Process.Kill()
			}
			select {
			case <-process.waitDone:
			case <-time.After(time.Second):
			}
		}
		stopCLIWireAudit(process.wireAudit)
	})
}
