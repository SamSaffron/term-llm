package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/mcphttp"
)

// mcpCallCounter generates unique IDs for MCP tool calls
var mcpCallCounter atomic.Int64

// ClaudeBinProvider implements Provider using the claude CLI binary.
// This provider shells out to the claude command for inference,
// using Claude Code's existing authentication.
//
// Note: This provider is NOT safe for concurrent use. Each Stream() call
// modifies shared state (sessionID, messagesSent). Create separate instances
// for concurrent streams.
type ClaudeBinProvider struct {
	model        string
	effort       string // reasoning effort for opus: "low", "medium", "high", "max", or ""
	sessionID    string // For session continuity with --resume
	messagesSent int    // Track messages already in session to avoid re-sending
	toolExecutor mcphttp.ToolExecutor
	preferOAuth  bool // If true, clear ANTHROPIC_API_KEY to force OAuth auth

	// Persistent MCP server for multi-turn conversations.
	// The server is kept alive across turns so Claude CLI can maintain
	// its connection to the same URL/token throughout the session.
	mcpServer     *mcphttp.Server
	mcpConfigPath string

	// currentEvents holds the events channel for the current turn.
	// currentBridge is updated at the start of each turn so the MCP executor
	// can route tool execution requests to the correct active stream.
	currentBridge *claudeTurnBridge
	// currentEvents is kept for fallback/direct execution paths.
	currentEvents chan<- Event
	eventsMu      sync.Mutex
}

type claudeToolRequest struct {
	ctx    context.Context
	callID string
	name   string
	args   json.RawMessage
	// response is completed by engine tool execution once EventToolCall is handled.
	response chan<- ToolExecutionResponse
	// ack is completed by the turn dispatcher after the request is either forwarded
	// to the stream events channel or rejected (stream closed/cancelled).
	ack chan error
}

type claudeTurnBridge struct {
	// toolReqCh routes wrapped MCP tool requests through the active turn dispatcher,
	// ensuring deterministic ordering relative to streamed stdout lines.
	toolReqCh chan claudeToolRequest
	// done closes when the active runClaudeCommand turn exits.
	done chan struct{}
}

const (
	claudeToolLineDrainGraceDefault = 75 * time.Millisecond
	claudeToolLineDrainGraceEnv     = "TERM_LLM_CLAUDE_TOOL_LINE_GRACE_MS"
)

var claudeToolLineDrainGrace = loadClaudeToolLineDrainGrace()

func loadClaudeToolLineDrainGrace() time.Duration {
	v := strings.TrimSpace(os.Getenv(claudeToolLineDrainGraceEnv))
	if v == "" {
		return claudeToolLineDrainGraceDefault
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < 0 {
		return claudeToolLineDrainGraceDefault
	}
	return time.Duration(ms) * time.Millisecond
}

// parseClaudeEffort extracts effort suffix from opus model names only.
// "opus-max" -> ("opus", "max"), "opus-low" -> ("opus", "low")
// "sonnet-max" -> ("sonnet-max", "") — non-opus models are not modified.
func parseClaudeEffort(model string) (string, string) {
	if !strings.HasPrefix(model, "opus") {
		return model, ""
	}
	for _, effort := range []string{"medium", "max", "high", "low"} {
		suffix := "-" + effort
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), effort
		}
	}
	return model, ""
}

// NewClaudeBinProvider creates a new provider that uses the claude binary.
func NewClaudeBinProvider(model string) *ClaudeBinProvider {
	actualModel, effort := parseClaudeEffort(model)
	return &ClaudeBinProvider{
		model:       actualModel,
		effort:      effort,
		preferOAuth: true, // Default to OAuth to avoid API key limits
	}
}

// SetPreferOAuth controls whether to prefer OAuth auth over API key.
// When true (default), clears ANTHROPIC_API_KEY for the subprocess
// so Claude CLI uses OAuth subscription auth instead.
func (p *ClaudeBinProvider) SetPreferOAuth(prefer bool) {
	p.preferOAuth = prefer
}

// SetToolExecutor sets the function used to execute tools.
// This must be called before Stream() if tools are needed.
// Note: The signature uses an anonymous function type (not mcphttp.ToolExecutor)
// to satisfy the ToolExecutorSetter interface in engine.go.
func (p *ClaudeBinProvider) SetToolExecutor(executor func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error)) {
	// Wrap the ToolOutput executor to satisfy the mcphttp.ToolExecutor (string, error) interface.
	// MCP tools only use the text content — diffs/images are forwarded via events.
	p.toolExecutor = func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		output, err := executor(ctx, name, args)
		return output.Content, err
	}
}

func (p *ClaudeBinProvider) Name() string {
	model := p.model
	if model == "" {
		model = "sonnet"
	}
	if p.effort != "" {
		return fmt.Sprintf("Claude CLI (%s, effort=%s)", model, p.effort)
	}
	return fmt.Sprintf("Claude CLI (%s)", model)
}

func (p *ClaudeBinProvider) Credential() string {
	return "claude-bin"
}

func (p *ClaudeBinProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    false, // Use term-llm's external tools instead
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: false, // Claude CLI doesn't support forcing specific tool use
		ManagesOwnContext:  true,  // Claude CLI handles its own context window management
	}
}

func (p *ClaudeBinProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		// Build the command arguments, passing events channel for tool execution routing.
		// MCP server is kept alive across turns - caller should call CleanupMCP() when done.
		args, effort := p.buildArgs(ctx, req, events)

		// Always extract system prompt from full messages (it should persist across turns)
		systemPrompt := p.extractSystemPrompt(req.Messages)

		// When resuming a session, only send new messages (claude CLI has the rest)
		messagesToSend := req.Messages
		if p.sessionID != "" && p.messagesSent > 0 && p.messagesSent < len(req.Messages) {
			messagesToSend = req.Messages[p.messagesSent:]
		}

		// Build the conversation prompt from messages to send
		userPrompt := p.buildConversationPrompt(messagesToSend)

		// Add system prompt if present
		if systemPrompt != "" {
			args = append(args, "--system-prompt", systemPrompt)
		}

		debug := req.Debug || req.DebugRaw

		err := p.runClaudeCommand(ctx, args, effort, userPrompt, debug, events)
		if err != nil && isPromptTooLong(err) {
			// Retry with progressively more aggressive truncation
			retryLimits := []int{maxToolResultCharsOnRetry, maxToolResultCharsOnAggressiveRetry}
			prevLen := len(userPrompt)
			for _, limit := range retryLimits {
				truncated := truncateToolResultsAt(messagesToSend, limit)
				retryPrompt := p.buildConversationPrompt(truncated)
				if len(retryPrompt) >= prevLen {
					slog.Warn("prompt too long but truncation did not reduce size, not retrying",
						"limit", limit)
					break
				}
				slog.Info("prompt too long, retrying with truncated tool results",
					"original_len", prevLen, "truncated_len", len(retryPrompt), "limit", limit)
				prevLen = len(retryPrompt)
				err = p.runClaudeCommand(ctx, args, effort, retryPrompt, debug, events)
				if err == nil || !isPromptTooLong(err) {
					break
				}
			}
		}
		if err != nil {
			return err
		}

		// Track messages sent so we don't re-send them on resume
		p.messagesSent = len(req.Messages)

		events <- Event{Type: EventDone}
		return nil
	}), nil
}

// runClaudeCommand executes the claude CLI binary with the given arguments and prompt,
// parsing its streaming JSON output into events. Returns nil on success.
func (p *ClaudeBinProvider) runClaudeCommand(
	ctx context.Context,
	args []string,
	effort string,
	userPrompt string,
	debug bool,
	events chan<- Event,
) error {
	// Note: We pass the prompt via stdin instead of command line args
	// to avoid "argument list too long" errors with large tool results (e.g., base64 images)

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Claude CLI Command ===")
		fmt.Fprintf(os.Stderr, "claude %s\n", strings.Join(args, " "))
		fmt.Fprintf(os.Stderr, "Prompt length: %d bytes (via stdin)\n", len(userPrompt))
		if effort != "" {
			fmt.Fprintf(os.Stderr, "CLAUDE_CODE_EFFORT_LEVEL=%s\n", effort)
		}
		fmt.Fprintln(os.Stderr, "=================================")
	}

	cmd := exec.CommandContext(ctx, "claude", args...)

	// Set up environment - optionally clear ANTHROPIC_API_KEY to prefer OAuth,
	// and set CLAUDE_CODE_EFFORT_LEVEL for reasoning effort control.
	if p.preferOAuth || effort != "" {
		env := os.Environ()
		filtered := env[:0]
		for _, e := range env {
			if p.preferOAuth && strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
				continue
			}
			if effort != "" && strings.HasPrefix(e, "CLAUDE_CODE_EFFORT_LEVEL=") {
				continue
			}
			filtered = append(filtered, e)
		}
		if effort != "" {
			filtered = append(filtered, "CLAUDE_CODE_EFFORT_LEVEL="+effort)
		}
		cmd.Env = filtered
	}

	// Set up stdin pipe for the prompt
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start claude: %w", err)
	}

	bridge := &claudeTurnBridge{
		toolReqCh: make(chan claudeToolRequest, 64),
		done:      make(chan struct{}),
	}
	p.eventsMu.Lock()
	p.currentBridge = bridge
	p.currentEvents = events
	p.eventsMu.Unlock()
	defer func() {
		p.eventsMu.Lock()
		if p.currentBridge == bridge {
			p.currentBridge = nil
			p.currentEvents = nil
		}
		p.eventsMu.Unlock()
		close(bridge.done)
	}()

	// Log stderr in background (claude CLI outputs progress/errors here)
	go func() {
		stderrScanner := bufio.NewScanner(stderr)
		stderrScanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for stderrScanner.Scan() {
			line := stderrScanner.Text()
			if debug {
				fmt.Fprintf(os.Stderr, "[claude stderr] %s\n", line)
			}
		}
	}()

	// Write prompt to stdin and close
	go func() {
		defer stdin.Close()
		stdin.Write([]byte(userPrompt))
	}()

	lineCh := make(chan string, 256)
	scanErrCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		// Increase buffer size for large JSON messages
		scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				select {
				case lineCh <- line:
				case <-bridge.done:
					close(lineCh)
					scanErrCh <- nil
					return
				case <-ctx.Done():
					close(lineCh)
					scanErrCh <- ctx.Err()
					return
				}
			}
		}
		close(lineCh)
		scanErrCh <- scanner.Err()
	}()

	lastUsage, err := p.dispatchClaudeEvents(ctx, lineCh, bridge.toolReqCh, debug, events)
	if err != nil {
		// Kill the process if dispatch failed (e.g., context cancelled)
		// to avoid orphan processes.
		cmd.Process.Kill()
	}

	// Wait for scanner to finish BEFORE cmd.Wait().
	// Go docs: "It is incorrect to call Wait before all reads from the pipe have completed."
	scanErr := <-scanErrCh

	// Now safe to call Wait() — all pipe reads are done.
	cmdErr := cmd.Wait()

	if err != nil {
		return err
	}
	if scanErr != nil {
		return fmt.Errorf("error reading claude output: %w", scanErr)
	}
	if cmdErr != nil {
		return fmt.Errorf("claude command failed: %w", cmdErr)
	}

	if lastUsage != nil {
		events <- Event{Type: EventUsage, Use: lastUsage}
	}
	return nil
}

func (p *ClaudeBinProvider) dispatchClaudeEvents(
	ctx context.Context,
	lineCh <-chan string,
	toolReqCh <-chan claudeToolRequest,
	debug bool,
	events chan<- Event,
) (*Usage, error) {
	var (
		lastUsage             *Usage
		linesOpen             = true
		sawTextDelta          bool
		assistantFallbackText string
	)

	for linesOpen {
		// Process all ready stdout lines first to preserve text/tool ordering.
		hadLine := false
		for linesOpen {
			select {
			case line, ok := <-lineCh:
				if !ok {
					linesOpen = false
					break
				}
				hadLine = true
				if err := p.handleClaudeLine(ctx, line, debug, events, &lastUsage, &sawTextDelta, &assistantFallbackText); err != nil {
					return nil, err
				}
			default:
				goto drainDone
			}
		}
	drainDone:
		if hadLine {
			continue
		}

		select {
		case line, ok := <-lineCh:
			if !ok {
				linesOpen = false
				continue
			}
			if err := p.handleClaudeLine(ctx, line, debug, events, &lastUsage, &sawTextDelta, &assistantFallbackText); err != nil {
				return nil, err
			}
		case req := <-toolReqCh:
			if err := p.drainClaudeLinesWithGrace(ctx, lineCh, debug, events, &lastUsage, &sawTextDelta, &assistantFallbackText); err != nil {
				return nil, err
			}
			p.handleClaudeToolRequest(req, events)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Drain any queued tool requests that arrived before stream shutdown.
	for {
		select {
		case req := <-toolReqCh:
			p.handleClaudeToolRequest(req, events)
		default:
			goto drained
		}
	}
drained:

	return lastUsage, nil
}

func (p *ClaudeBinProvider) handleClaudeLine(
	ctx context.Context,
	line string,
	debug bool,
	events chan<- Event,
	lastUsage **Usage,
	sawTextDelta *bool,
	assistantFallbackText *string,
) error {
	var baseMsg struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &baseMsg); err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "Failed to parse JSON: %s\n", line[:min(100, len(line))])
		}
		return nil
	}

	switch baseMsg.Type {
	case "system":
		// Extract session ID for potential resume
		var sysMsg claudeSystemMessage
		if err := json.Unmarshal([]byte(line), &sysMsg); err == nil {
			p.sessionID = sysMsg.SessionID
			if debug {
				fmt.Fprintf(os.Stderr, "Session: %s, Model: %s, Tools: %v\n",
					sysMsg.SessionID, sysMsg.Model, sysMsg.Tools)
			}
		}

	case "stream_event":
		// Handle streaming text deltas
		var streamEvent claudeStreamEvent
		if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
			return nil
		}
		if streamEvent.Event.Type == "content_block_delta" &&
			streamEvent.Event.Delta.Type == "text_delta" &&
			streamEvent.Event.Delta.Text != "" {
			if !safeSendEvent(ctx, events, Event{Type: EventTextDelta, Text: streamEvent.Event.Delta.Text}) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("failed to emit claude text delta: stream closed")
			}
			if sawTextDelta != nil {
				*sawTextDelta = true
			}
		}

	case "assistant":
		// Buffer assistant text as a fallback for providers/versions that
		// don't emit stream_event text deltas.
		var assistantMsg claudeAssistantMessage
		if err := json.Unmarshal([]byte(line), &assistantMsg); err == nil && assistantFallbackText != nil {
			if text := extractClaudeAssistantText(assistantMsg); text != "" {
				*assistantFallbackText = text
			}
		}

	case "result":
		var resultMsg claudeResultMessage
		if err := json.Unmarshal([]byte(line), &resultMsg); err == nil {
			// Check for API errors (rate limits, auth issues, etc.)
			if resultMsg.IsError && resultMsg.Result != "" {
				return fmt.Errorf("claude API error: %s", resultMsg.Result)
			}
			if sawTextDelta != nil && assistantFallbackText != nil && !*sawTextDelta && strings.TrimSpace(*assistantFallbackText) != "" {
				if !safeSendEvent(ctx, events, Event{Type: EventTextDelta, Text: *assistantFallbackText}) {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return fmt.Errorf("failed to emit claude assistant fallback text: stream closed")
				}
				*sawTextDelta = true
			}
			*lastUsage = &Usage{
				InputTokens:  resultMsg.Usage.InputTokens + resultMsg.Usage.CacheReadInputTokens,
				OutputTokens: resultMsg.Usage.OutputTokens,
			}
		}
	}

	return nil
}

func (p *ClaudeBinProvider) handleClaudeToolRequest(req claudeToolRequest, events chan<- Event) {
	event := Event{
		Type:         EventToolCall,
		ToolCallID:   req.callID,
		ToolName:     req.name,
		Tool:         &ToolCall{ID: req.callID, Name: req.name, Arguments: req.args},
		ToolResponse: req.response,
	}

	if !safeSendEvent(req.ctx, events, event) {
		if req.ctx.Err() != nil {
			req.ack <- req.ctx.Err()
			return
		}
		req.ack <- fmt.Errorf("tool execution rejected: stream closed during tool call %q", req.name)
		return
	}
	req.ack <- nil
}

func (p *ClaudeBinProvider) drainClaudeLinesWithGrace(
	ctx context.Context,
	lineCh <-chan string,
	debug bool,
	events chan<- Event,
	lastUsage **Usage,
	sawTextDelta *bool,
	assistantFallbackText *string,
) error {
	// First, drain any already-buffered lines.
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return nil
			}
			if err := p.handleClaudeLine(ctx, line, debug, events, lastUsage, sawTextDelta, assistantFallbackText); err != nil {
				return err
			}
		default:
			goto wait
		}
	}

wait:
	timer := time.NewTimer(claudeToolLineDrainGrace)
	defer timer.Stop()

	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return nil
			}
			if err := p.handleClaudeLine(ctx, line, debug, events, lastUsage, sawTextDelta, assistantFallbackText); err != nil {
				return err
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(claudeToolLineDrainGrace)
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// buildArgs constructs the command line arguments for the claude binary.
// The events channel is passed to the MCP server for routing tool execution events.
// The MCP server is kept alive across turns - call CleanupMCP() when the conversation ends.
// Returns the args and the effective reasoning effort (if any).
func (p *ClaudeBinProvider) buildArgs(ctx context.Context, req Request, events chan<- Event) ([]string, string) {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--include-partial-messages", // Stream text as it arrives
		"--verbose",
		"--strict-mcp-config",            // Ignore Claude's configured MCPs
		"--dangerously-skip-permissions", // Allow MCP tool execution
		"--setting-sources", "user",      // Skip project CLAUDE.md files (term-llm provides its own context)
	}

	// Always limit to 1 turn - term-llm handles tool execution loop
	args = append(args, "--max-turns", "1")

	// Model selection — parse effort from the chosen model, fall back to provider-level effort
	model := chooseModel(req.Model, p.model)
	strippedModel, reqEffort := parseClaudeEffort(model)
	effort := p.effort
	if effort == "" && reqEffort != "" {
		effort = reqEffort
	}
	if strippedModel != "" {
		args = append(args, "--model", mapModelToClaudeArg(strippedModel))
	}

	// Disable all built-in tools - we use MCP for custom tools
	args = append(args, "--tools", "")

	// If we have tools and a tool executor, use persistent MCP server
	debug := req.Debug || req.DebugRaw
	if len(req.Tools) > 0 {
		if p.toolExecutor == nil {
			slog.Warn("tools requested but no tool executor configured", "tool_count", len(req.Tools))
		} else {
			// Reuse existing MCP server if available, otherwise create new one
			mcpConfig := p.getOrCreateMCPConfig(ctx, req.Tools, events, debug)
			if mcpConfig != "" {
				args = append(args, "--mcp-config", mcpConfig)
			} else if debug {
				fmt.Fprintf(os.Stderr, "[claude-bin] ERROR: MCP config creation failed\n")
			}
		}
	}

	// Session resume for multi-turn conversations
	if p.sessionID != "" {
		args = append(args, "--resume", p.sessionID)
	}

	return args, effort
}

// getOrCreateMCPConfig returns the MCP config path, reusing existing server if available.
// This ensures the MCP server URL/token stays constant across turns in a multi-turn conversation.
func (p *ClaudeBinProvider) getOrCreateMCPConfig(ctx context.Context, tools []ToolSpec, events chan<- Event, debug bool) string {
	_ = events

	// If we already have a running MCP server, reuse its config
	if p.mcpServer != nil && p.mcpConfigPath != "" {
		if debug {
			fmt.Fprintf(os.Stderr, "[claude-bin] Reusing existing MCP server at %s\n", p.mcpServer.URL())
		}
		return p.mcpConfigPath
	}

	// Create new MCP server
	if debug {
		fmt.Fprintf(os.Stderr, "[claude-bin] Starting HTTP MCP server for %d tools\n", len(tools))
	}

	configPath := p.createHTTPMCPConfig(ctx, tools, debug)
	if configPath != "" && debug {
		fmt.Fprintf(os.Stderr, "[claude-bin] MCP config created: %s\n", configPath)
	}
	return configPath
}

// CleanupMCP stops the MCP server and removes the config file.
// This should be called when the conversation is complete.
func (p *ClaudeBinProvider) CleanupMCP() {
	if p.mcpServer != nil {
		p.mcpServer.Stop(context.Background())
		p.mcpServer = nil
	}
	if p.mcpConfigPath != "" {
		os.Remove(p.mcpConfigPath)
		p.mcpConfigPath = ""
	}
}

// createHTTPMCPConfig starts an HTTP MCP server and creates a config file pointing to it.
// The server and config path are stored in the provider for reuse across turns.
// Tool execution events are routed through p.currentEvents (set by getOrCreateMCPConfig).
func (p *ClaudeBinProvider) createHTTPMCPConfig(ctx context.Context, tools []ToolSpec, debug bool) string {
	// Convert llm.ToolSpec to mcphttp.ToolSpec
	mcpTools := make([]mcphttp.ToolSpec, len(tools))
	for i, t := range tools {
		mcpTools[i] = mcphttp.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
		}
		if debug {
			fmt.Fprintf(os.Stderr, "[claude-bin] Registering tool: %s\n", t.Name)
		}
	}

	// Create a wrapper executor that routes tool calls through the engine
	// by emitting EventToolCall with a response channel and waiting for the result.
	// NOTE: We read p.currentBridge/currentEvents under mutex each time to get the
	// current turn's stream sink. This is critical because the MCP server persists
	// across turns but the stream channels change every turn.
	wrappedExecutor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		// Get the current bridge/events for this turn.
		p.eventsMu.Lock()
		bridge := p.currentBridge
		events := p.currentEvents
		p.eventsMu.Unlock()

		// If no active stream bridge/events channel, reject execution.
		// Falling back to direct execution here would bypass stream-level sequencing
		// and could skip expected UI/event handling semantics.
		if bridge == nil || events == nil {
			return "", fmt.Errorf("tool execution rejected: no active stream bridge for tool call %q", name)
		}

		// Generate a unique call ID for this execution
		callID := fmt.Sprintf("mcp-%s-%d", name, mcpCallCounter.Add(1))

		// Create response channel for synchronous execution
		responseChan := make(chan ToolExecutionResponse, 1)

		req := claudeToolRequest{
			ctx:      ctx,
			callID:   callID,
			name:     name,
			args:     args,
			response: responseChan,
			ack:      make(chan error, 1),
		}

		// Route tool request through the turn bridge so ordering is handled centrally.
		select {
		case bridge.toolReqCh <- req:
		case <-bridge.done:
			return "", fmt.Errorf("tool execution rejected: stream closed during tool call %q", name)
		case <-ctx.Done():
			return "", ctx.Err()
		}

		select {
		case err := <-req.ack:
			if err != nil {
				return "", err
			}
		case <-bridge.done:
			return "", fmt.Errorf("tool execution rejected: stream closed during tool call %q", name)
		case <-ctx.Done():
			return "", ctx.Err()
		}

		// Wait for engine to execute and return result
		select {
		case response := <-responseChan:
			return response.Result.Content, response.Err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	// Create and start HTTP server
	server := mcphttp.NewServer(wrappedExecutor)
	server.SetDebug(debug)
	url, token, err := server.Start(ctx, mcpTools)
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[claude-bin] Failed to start MCP server: %v\n", err)
		}
		return ""
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[claude-bin] MCP server started at %s\n", url)
	}

	// Create MCP config with HTTP URL
	// Note: "type": "http" is required for Claude Code to use HTTP transport
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"term-llm": map[string]any{
				"type": "http",
				"url":  url,
				"headers": map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}

	configJSON, err := json.Marshal(mcpConfig)
	if err != nil {
		server.Stop(ctx)
		return ""
	}

	// Write to temp file using os.CreateTemp to avoid symlink attacks
	tmpFile, err := os.CreateTemp("", "term-llm-mcp-*.json")
	if err != nil {
		server.Stop(ctx)
		return ""
	}
	configPath := tmpFile.Name()
	if _, err := tmpFile.Write(configJSON); err != nil {
		tmpFile.Close()
		os.Remove(configPath)
		server.Stop(ctx)
		return ""
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(configPath)
		server.Stop(ctx)
		return ""
	}

	// Store server and config for reuse across turns
	p.mcpServer = server
	p.mcpConfigPath = configPath

	return configPath
}

// extractSystemPrompt extracts system messages from the full message list.
// This should always be called with the complete messages to ensure the system
// prompt persists across turns in multi-turn conversations.
func (p *ClaudeBinProvider) extractSystemPrompt(messages []Message) string {
	var systemParts []string
	for _, msg := range messages {
		if msg.Role == RoleSystem {
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		}
	}
	return strings.TrimSpace(strings.Join(systemParts, "\n\n"))
}

// buildConversationPrompt constructs the conversation prompt from messages.
// This can be called with a subset of messages when resuming a session.
func (p *ClaudeBinProvider) buildConversationPrompt(messages []Message) string {
	var conversationParts []string

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			// System messages handled separately by extractSystemPrompt
			continue
		case RoleUser:
			text := collectTextParts(msg.Parts)
			if text != "" {
				conversationParts = append(conversationParts, "User: "+text)
			}
		case RoleAssistant:
			text := collectTextParts(msg.Parts)
			// Also capture tool calls from assistant
			for _, part := range msg.Parts {
				if part.Type == PartToolCall && part.ToolCall != nil {
					conversationParts = append(conversationParts,
						fmt.Sprintf("Assistant called tool: %s", part.ToolCall.Name))
				}
			}
			if text != "" {
				conversationParts = append(conversationParts, "Assistant: "+text)
			}
		case RoleTool:
			// Format tool results
			for _, part := range msg.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil {
					// Process content to keep prompts compact for image tool results.
					content := p.processToolResultContent(part.ToolResult)
					conversationParts = append(conversationParts,
						fmt.Sprintf("Tool result (%s): %s", part.ToolResult.Name, content))
				}
			}
		}
	}

	return strings.TrimSpace(strings.Join(conversationParts, "\n\n"))
}

// mapModelToClaudeArg converts a model name to claude CLI argument.
func mapModelToClaudeArg(model string) string {
	model = strings.ToLower(model)
	if strings.Contains(model, "opus") {
		return "opus"
	}
	if strings.Contains(model, "haiku") {
		return "haiku"
	}
	// Default to sonnet
	return "sonnet"
}

// mapClaudeToolName converts claude tool names back to term-llm names.
// MCP tools are namespaced as mcp__term-llm__<tool>.
func mapClaudeToolName(claudeName string) string {
	if strings.HasPrefix(claudeName, "mcp__term-llm__") {
		return strings.TrimPrefix(claudeName, "mcp__term-llm__")
	}
	return claudeName
}

// processToolResultContent keeps tool results compact for Claude CLI prompts.
// Structured image_data parts are intentionally omitted from the prompt text.
func (p *ClaudeBinProvider) processToolResultContent(result *ToolResult) string {
	if result == nil {
		return ""
	}

	textContent := toolResultTextContent(result)
	if !toolResultHasImageData(result) {
		return textContent
	}

	notice := "[Image data omitted from Claude CLI prompt]"
	if strings.TrimSpace(textContent) == "" {
		return notice
	}
	return strings.TrimSpace(textContent + "\n" + notice)
}

func extractClaudeAssistantText(msg claudeAssistantMessage) string {
	var b strings.Builder
	for _, block := range msg.Message.Content {
		if block.Type == "text" && block.Text != "" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// safeSendEvent attempts to send an event to the channel, returning false if
// the channel is closed or context is cancelled. This prevents panics when
// the stream is closed while MCP tool execution is still in progress.
func safeSendEvent(ctx context.Context, ch chan<- Event, event Event) (sent bool) {
	// Recover from panic if channel is closed
	defer func() {
		if r := recover(); r != nil {
			sent = false
		}
	}()

	select {
	case ch <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// isPromptTooLong checks whether the error from claude CLI indicates the
// prompt exceeded the model's context window.
func isPromptTooLong(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "prompt is too long")
}

// maxToolResultCharsOnRetry is the maximum character length for each tool result
// when retrying after a "prompt too long" error (~5.7K tokens at 3.5 chars/token).
const maxToolResultCharsOnRetry = 20_000

// maxToolResultCharsOnAggressiveRetry is used for the second retry with much
// more aggressive truncation (~1.4K tokens at 3.5 chars/token).
const maxToolResultCharsOnAggressiveRetry = 5_000

// truncateToolResultsAt returns a copy of messages with oversized tool result
// content truncated to maxChars runes.
// Note: only copies Role and Parts — update if Message gains new fields.
func truncateToolResultsAt(messages []Message, maxChars int) []Message {
	out := make([]Message, len(messages))
	for i, msg := range messages {
		out[i] = Message{Role: msg.Role}
		out[i].Parts = make([]Part, len(msg.Parts))
		for j, part := range msg.Parts {
			out[i].Parts[j] = part
			if part.Type == PartToolResult && part.ToolResult != nil {
				content := part.ToolResult.Content
				runes := []rune(content)
				if len(runes) > maxChars {
					truncated := string(runes[:maxChars])
					truncated += fmt.Sprintf("\n[Truncated: showing first %d of %d chars]",
						maxChars, len(runes))
					// Clone ToolResult to avoid mutating original
					tr := *part.ToolResult
					tr.Content = truncated
					out[i].Parts[j].ToolResult = &tr
				}
			}
		}
	}
	return out
}

// JSON message types from claude CLI output

type claudeSystemMessage struct {
	Type      string   `json:"type"`
	SessionID string   `json:"session_id"`
	Model     string   `json:"model"`
	Tools     []string `json:"tools"`
}

type claudeAssistantMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content []claudeContentBlock `json:"content"`
	} `json:"message"`
}

type claudeContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type claudeResultMessage struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

type claudeStreamEvent struct {
	Type  string `json:"type"`
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}
