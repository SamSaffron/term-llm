package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ClaudeBinProvider implements Provider using the claude CLI binary.
// This provider shells out to the claude command for inference,
// using Claude Code's existing authentication.
type ClaudeBinProvider struct {
	model        string
	sessionID    string // For session continuity with --resume
	messagesSent int    // Track messages already in session to avoid re-sending
}

// NewClaudeBinProvider creates a new provider that uses the claude binary.
func NewClaudeBinProvider(model string) *ClaudeBinProvider {
	return &ClaudeBinProvider{
		model: model,
	}
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
		// Build the command arguments
		args, cleanup := p.buildArgs(req)
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

		if req.Debug {
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

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start claude: %w", err)
		}

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
				if req.Debug {
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
					if req.Debug {
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
// Returns args and a cleanup function to remove temp files.
func (p *ClaudeBinProvider) buildArgs(req Request) ([]string, func()) {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--include-partial-messages", // Stream text as it arrives
		"--verbose",
		"--strict-mcp-config",            // Ignore Claude's configured MCPs
		"--dangerously-skip-permissions", // Allow MCP tool execution
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

	// If we have tools, create MCP config to expose them
	if len(req.Tools) > 0 {
		mcpConfig, cleanupFn := p.createMCPConfig(req.Tools)
		if mcpConfig != "" {
			args = append(args, "--mcp-config", mcpConfig)
			cleanup = cleanupFn
		}
	}

	// Session resume for multi-turn conversations
	if p.sessionID != "" {
		args = append(args, "--resume", p.sessionID)
	}

	return args, cleanup
}

// createMCPConfig creates a temporary MCP config file for the given tools.
// Returns the config file path and a cleanup function.
func (p *ClaudeBinProvider) createMCPConfig(tools []ToolSpec) (string, func()) {
	// Get the path to term-llm binary
	execPath, err := os.Executable()
	if err != nil {
		return "", nil
	}

	// Build tool definitions JSON
	type toolDef struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Schema      map[string]any `json:"schema"`
	}
	var toolDefs []toolDef
	for _, t := range tools {
		toolDefs = append(toolDefs, toolDef{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
		})
	}

	toolsJSON, err := json.Marshal(toolDefs)
	if err != nil {
		return "", nil
	}

	// Create MCP config
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"term-llm": map[string]any{
				"command": execPath,
				"args":    []string{"mcp-server", "--tools-json", string(toolsJSON)},
			},
		},
	}

	configJSON, err := json.Marshal(mcpConfig)
	if err != nil {
		return "", nil
	}

	// Write to temp file
	tmpDir := os.TempDir()
	configPath := filepath.Join(tmpDir, fmt.Sprintf("term-llm-mcp-%d.json", os.Getpid()))
	if err := os.WriteFile(configPath, configJSON, 0600); err != nil {
		return "", nil
	}

	cleanup := func() {
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
