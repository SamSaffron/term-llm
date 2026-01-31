package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

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
	// This is updated at the start of each turn so the MCP executor
	// can send events to the correct channel (not a stale closed one).
	currentEvents chan<- Event
	eventsMu      sync.Mutex
}

// NewClaudeBinProvider creates a new provider that uses the claude binary.
func NewClaudeBinProvider(model string) *ClaudeBinProvider {
	return &ClaudeBinProvider{
		model:       model,
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
func (p *ClaudeBinProvider) SetToolExecutor(executor func(ctx context.Context, name string, args json.RawMessage) (string, error)) {
	p.toolExecutor = executor
}

func (p *ClaudeBinProvider) Name() string {
	model := p.model
	if model == "" {
		model = "sonnet"
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
	}
}

func (p *ClaudeBinProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		// Build the command arguments, passing events channel for tool execution routing.
		// MCP server is kept alive across turns - caller should call CleanupMCP() when done.
		args := p.buildArgs(ctx, req, events)

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

		// Note: We pass the prompt via stdin instead of command line args
		// to avoid "argument list too long" errors with large tool results (e.g., base64 images)

		debug := req.Debug || req.DebugRaw
		if debug {
			fmt.Fprintln(os.Stderr, "=== DEBUG: Claude CLI Command ===")
			fmt.Fprintf(os.Stderr, "claude %s\n", strings.Join(args, " "))
			fmt.Fprintf(os.Stderr, "Prompt length: %d bytes (via stdin)\n", len(userPrompt))
			fmt.Fprintln(os.Stderr, "=================================")
		}

		cmd := exec.CommandContext(ctx, "claude", args...)

		// Set up environment - optionally clear ANTHROPIC_API_KEY to prefer OAuth
		if p.preferOAuth {
			env := os.Environ()
			filtered := env[:0]
			for _, e := range env {
				if !strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
					filtered = append(filtered, e)
				}
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

		// Log stderr in background (claude CLI outputs progress/errors here)
		go func() {
			stderrScanner := bufio.NewScanner(stderr)
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

		scanner := bufio.NewScanner(stdout)
		// Increase buffer size for large JSON messages
		scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

		var lastUsage *Usage

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			// Parse the message type first
			var baseMsg struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(line), &baseMsg); err != nil {
				if debug {
					fmt.Fprintf(os.Stderr, "Failed to parse JSON: %s\n", line[:min(100, len(line))])
				}
				continue
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
					continue
				}
				if streamEvent.Event.Type == "content_block_delta" &&
					streamEvent.Event.Delta.Type == "text_delta" &&
					streamEvent.Event.Delta.Text != "" {
					events <- Event{Type: EventTextDelta, Text: streamEvent.Event.Delta.Text}
				}

			case "assistant":
				// Tool execution is handled via MCP HTTP path (wrappedExecutor).
				// The "assistant" message is output BEFORE claude calls MCP,
				// so we can't use it for tracking. Just ignore it - MCP handles everything.

			case "result":
				var resultMsg claudeResultMessage
				if err := json.Unmarshal([]byte(line), &resultMsg); err == nil {
					// Check for API errors (rate limits, auth issues, etc.)
					if resultMsg.IsError && resultMsg.Result != "" {
						return fmt.Errorf("%s", resultMsg.Result)
					}
					lastUsage = &Usage{
						InputTokens:  resultMsg.Usage.InputTokens + resultMsg.Usage.CacheReadInputTokens,
						OutputTokens: resultMsg.Usage.OutputTokens,
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			return fmt.Errorf("error reading claude output: %w", err)
		}

		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("claude command failed: %w", err)
		}

		// Track messages sent so we don't re-send them on resume
		p.messagesSent = len(req.Messages)

		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

// buildArgs constructs the command line arguments for the claude binary.
// The events channel is passed to the MCP server for routing tool execution events.
// The MCP server is kept alive across turns - call CleanupMCP() when the conversation ends.
func (p *ClaudeBinProvider) buildArgs(ctx context.Context, req Request, events chan<- Event) []string {
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

	// Model selection
	model := chooseModel(req.Model, p.model)
	if model != "" {
		args = append(args, "--model", mapModelToClaudeArg(model))
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

	return args
}

// getOrCreateMCPConfig returns the MCP config path, reusing existing server if available.
// This ensures the MCP server URL/token stays constant across turns in a multi-turn conversation.
func (p *ClaudeBinProvider) getOrCreateMCPConfig(ctx context.Context, tools []ToolSpec, events chan<- Event, debug bool) string {
	// Always update the current events channel for this turn.
	// This is critical: the MCP executor uses p.currentEvents, so we must
	// update it before any tool execution happens this turn.
	p.eventsMu.Lock()
	p.currentEvents = events
	p.eventsMu.Unlock()

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
	// NOTE: We read p.currentEvents under mutex each time to get the current turn's
	// channel. This is critical because the MCP server persists across turns but
	// the events channel changes with each turn.
	wrappedExecutor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		// Get the current events channel for this turn
		p.eventsMu.Lock()
		events := p.currentEvents
		p.eventsMu.Unlock()

		// If no events channel, fall back to direct execution
		if events == nil {
			return p.toolExecutor(ctx, name, args)
		}

		// Generate a unique call ID for this execution
		callID := fmt.Sprintf("mcp-%s-%d", name, mcpCallCounter.Add(1))

		// Create response channel for synchronous execution
		responseChan := make(chan ToolExecutionResponse, 1)

		// Emit EventToolCall with response channel - engine will execute and respond
		event := Event{
			Type:         EventToolCall,
			ToolCallID:   callID,
			ToolName:     name,
			Tool:         &ToolCall{ID: callID, Name: name, Arguments: args},
			ToolResponse: responseChan,
		}

		// Try to send the event. The channel may be closed if the stream ended
		// between when we grabbed it and now (race condition with stream cleanup).
		// Use safeSendEvent to handle this gracefully.
		if !safeSendEvent(ctx, events, event) {
			// Channel closed or context cancelled - do NOT fall back to direct execution
			// as that would bypass engine-level permission checks (allow-list + UI approval).
			// Return an error instead to signal the tool call cannot be processed safely.
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("tool execution rejected: stream closed during tool call %q", name)
		}

		// Wait for engine to execute and return result
		select {
		case response := <-responseChan:
			return response.Result, response.Err
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
					// Process content to handle embedded images
					content := p.processToolResultContent(part.ToolResult.Content)
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

// processToolResultContent handles embedded image data in tool results.
// It strips [IMAGE_DATA:mime:base64] markers since Claude CLI can read images
// natively from the file path that's already in the text part of the result.
func (p *ClaudeBinProvider) processToolResultContent(content string) string {
	const prefix = "[IMAGE_DATA:"
	const suffix = "]"

	start := strings.Index(content, prefix)
	if start == -1 {
		return content
	}

	end := strings.Index(content[start:], suffix)
	if end == -1 {
		return content
	}

	// Strip the image data marker - the file path is already in the text
	// (e.g., "Image loaded: /path/to/file.png") and Claude CLI can read it natively
	imageMarker := content[start : start+end+1]
	result := strings.Replace(content, imageMarker, "[Image data stripped - Claude can read the file path above]", 1)
	return strings.TrimSpace(result)
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
