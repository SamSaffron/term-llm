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
}

// NewClaudeBinProvider creates a new provider that uses the claude binary.
func NewClaudeBinProvider(model string) *ClaudeBinProvider {
	return &ClaudeBinProvider{
		model: model,
	}
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
		NativeWebSearch: false, // Use term-llm's external tools instead
		NativeWebFetch:  false,
		ToolCalls:       true,
	}
}

func (p *ClaudeBinProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		// Build the command arguments, passing events channel for tool execution routing
		args, cleanup := p.buildArgs(ctx, req, events)
		if cleanup != nil {
			defer cleanup()
		}

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
				// Only handle tool_use here - text is streamed via stream_event
				var assistantMsg claudeAssistantMessage
				if err := json.Unmarshal([]byte(line), &assistantMsg); err != nil {
					continue
				}

				for _, content := range assistantMsg.Message.Content {
					switch content.Type {
					case "tool_use":
						// Convert to term-llm tool call format
						toolCall := ToolCall{
							ID:        content.ID,
							Name:      mapClaudeToolName(content.Name),
							Arguments: content.Input,
						}

						events <- Event{Type: EventToolCall, Tool: &toolCall}
					}
				}

			case "result":
				var resultMsg claudeResultMessage
				if err := json.Unmarshal([]byte(line), &resultMsg); err == nil {
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
// Returns args and a cleanup function to stop HTTP server and remove temp files.
// The events channel is passed to the MCP server for routing tool execution events.
func (p *ClaudeBinProvider) buildArgs(ctx context.Context, req Request, events chan<- Event) ([]string, func()) {
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

	var cleanup func()

	// If we have tools and a tool executor, start HTTP MCP server
	debug := req.Debug || req.DebugRaw
	if len(req.Tools) > 0 {
		if p.toolExecutor == nil {
			slog.Warn("tools requested but no tool executor configured", "tool_count", len(req.Tools))
		} else {
			if debug {
				fmt.Fprintf(os.Stderr, "[claude-bin] Starting HTTP MCP server for %d tools\n", len(req.Tools))
			}
			mcpConfig, cleanupFn := p.createHTTPMCPConfig(ctx, req.Tools, events, debug)
			if mcpConfig != "" {
				if debug {
					fmt.Fprintf(os.Stderr, "[claude-bin] MCP config created: %s\n", mcpConfig)
				}
				args = append(args, "--mcp-config", mcpConfig)
				cleanup = cleanupFn
			} else if debug {
				fmt.Fprintf(os.Stderr, "[claude-bin] ERROR: MCP config creation failed\n")
			}
		}
	}

	// Session resume for multi-turn conversations
	if p.sessionID != "" {
		args = append(args, "--resume", p.sessionID)
	}

	return args, cleanup
}

// createHTTPMCPConfig starts an HTTP MCP server and creates a config file pointing to it.
// Returns the config file path and a cleanup function that stops the server.
// The events channel is used for routing tool execution events back to the engine.
func (p *ClaudeBinProvider) createHTTPMCPConfig(ctx context.Context, tools []ToolSpec, events chan<- Event, debug bool) (string, func()) {
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
	// Note: events is captured from the function parameter, making this safe for concurrent streams.
	wrappedExecutor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		// If no events channel, fall back to direct execution
		if events == nil {
			return p.toolExecutor(ctx, name, args)
		}

		// Generate a unique call ID for this execution
		callID := fmt.Sprintf("mcp-%s-%d", name, mcpCallCounter.Add(1))

		// Create response channel for synchronous execution
		responseChan := make(chan ToolExecutionResponse, 1)

		// Emit EventToolCall with response channel - engine will execute and respond
		// Use select to avoid blocking forever if the engine loop has exited
		event := Event{
			Type:         EventToolCall,
			ToolCallID:   callID,
			ToolName:     name,
			ToolInfo:     formatMCPToolArgs(args),
			Tool:         &ToolCall{ID: callID, Name: name, Arguments: args},
			ToolResponse: responseChan,
		}
		select {
		case events <- event:
			// Event sent successfully
		case <-ctx.Done():
			return "", ctx.Err()
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
		return "", nil
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
		return "", nil
	}

	// Write to temp file using os.CreateTemp to avoid symlink attacks
	tmpFile, err := os.CreateTemp("", "term-llm-mcp-*.json")
	if err != nil {
		server.Stop(ctx)
		return "", nil
	}
	configPath := tmpFile.Name()
	if _, err := tmpFile.Write(configJSON); err != nil {
		tmpFile.Close()
		os.Remove(configPath)
		server.Stop(ctx)
		return "", nil
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(configPath)
		server.Stop(ctx)
		return "", nil
	}

	cleanup := func() {
		server.Stop(context.Background())
		os.Remove(configPath)
	}

	return configPath, cleanup
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

// formatMCPToolArgs creates a preview string for MCP tool arguments.
func formatMCPToolArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	return formatToolArgs(m, 200, 3)
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
	Type  string `json:"type"`
	Usage struct {
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
