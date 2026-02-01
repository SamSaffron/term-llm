package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ExportOptions configures session export.
type ExportOptions struct {
	IncludeSystem bool // Include system prompt in export
}

// escapeTableCell escapes special characters for markdown table cells.
func escapeTableCell(s string) string {
	// Replace pipe characters and newlines which break tables
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// ExportToMarkdown exports a session and its messages to a pretty markdown format.
func ExportToMarkdown(sess *Session, messages []Message, opts ExportOptions) string {
	var b strings.Builder

	// Title
	title := sess.Name
	if title == "" {
		title = ShortID(sess.ID)
	}
	b.WriteString(fmt.Sprintf("# Session: %s\n\n", escapeTableCell(title)))

	// Header with link
	b.WriteString("> Exported from [term-llm](https://github.com/samsaffron/term-llm)\n\n")

	// Setup section
	b.WriteString("## Setup\n\n")
	b.WriteString("| | |\n")
	b.WriteString("|---|---|\n")

	// Agent (if set)
	if sess.Agent != "" {
		b.WriteString(fmt.Sprintf("| **Agent** | %s |\n", escapeTableCell(sess.Agent)))
	}

	// Provider and model
	b.WriteString(fmt.Sprintf("| **Provider** | %s |\n", escapeTableCell(sess.Provider)))
	b.WriteString(fmt.Sprintf("| **Model** | %s |\n", escapeTableCell(sess.Model)))

	// Mode
	mode := string(sess.Mode)
	if mode == "" {
		mode = "chat"
	}
	b.WriteString(fmt.Sprintf("| **Mode** | %s |\n", mode))

	// Created at
	b.WriteString(fmt.Sprintf("| **Created** | %s |\n", sess.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")))

	// Working directory
	if sess.CWD != "" {
		b.WriteString(fmt.Sprintf("| **Working Directory** | `%s` |\n", escapeTableCell(sess.CWD)))
	}

	b.WriteString("\n")

	// Tools (if set)
	if sess.Tools != "" {
		b.WriteString("<details>\n")
		b.WriteString("<summary>Tools</summary>\n\n")
		b.WriteString(sess.Tools)
		b.WriteString("\n\n</details>\n\n")
	}

	// MCP servers (if set)
	if sess.MCP != "" {
		b.WriteString("<details>\n")
		b.WriteString("<summary>MCP Servers</summary>\n\n")
		b.WriteString(sess.MCP)
		b.WriteString("\n\n</details>\n\n")
	}

	// Metrics section
	b.WriteString("## Metrics\n\n")
	b.WriteString("| Turns | Tool Calls | Tokens |\n")
	b.WriteString("|-------|------------|--------|\n")
	tokens := formatTokens(sess.InputTokens, sess.OutputTokens)
	b.WriteString(fmt.Sprintf("| %d user / %d LLM | %d | %s |\n\n",
		sess.UserTurns, sess.LLMTurns, sess.ToolCalls, tokens))

	b.WriteString("---\n\n")

	// Conversation section
	b.WriteString("## Conversation\n\n")

	// Track tool calls to pair with their results
	// We buffer tool calls and only render when we have the result
	toolCalls := make(map[string]*llm.ToolCall)

	// Buffer for pending content within an assistant turn
	// Tool calls are rendered inline when their results arrive
	var pendingText strings.Builder
	var pendingToolCalls []*llm.ToolCall
	inAssistantTurn := false

	flushAssistant := func() {
		if !inAssistantTurn {
			return
		}
		// Write any buffered text
		if pendingText.Len() > 0 {
			b.WriteString(pendingText.String())
			pendingText.Reset()
		}
		// Write any orphan tool calls (no results received)
		for _, tc := range pendingToolCalls {
			if _, exists := toolCalls[tc.ID]; exists {
				writeToolCall(&b, tc, nil)
				delete(toolCalls, tc.ID)
			}
		}
		pendingToolCalls = nil
		b.WriteString("---\n\n")
		inAssistantTurn = false
	}

	for _, msg := range messages {
		// Skip system messages unless explicitly included
		if msg.Role == llm.RoleSystem {
			if opts.IncludeSystem {
				flushAssistant()
				b.WriteString("### System\n\n")
				b.WriteString(msg.TextContent)
				b.WriteString("\n\n---\n\n")
			}
			continue
		}

		// Handle user messages
		if msg.Role == llm.RoleUser {
			flushAssistant()
			b.WriteString("### User\n\n")
			b.WriteString(msg.TextContent)
			b.WriteString("\n\n---\n\n")
			continue
		}

		// Handle assistant messages
		if msg.Role == llm.RoleAssistant {
			flushAssistant()
			inAssistantTurn = true
			b.WriteString("### Assistant\n\n")

			for _, part := range msg.Parts {
				if part.Type == llm.PartText && part.Text != "" {
					pendingText.WriteString(part.Text)
					pendingText.WriteString("\n\n")
				}
				if part.Type == llm.PartToolCall && part.ToolCall != nil {
					pendingToolCalls = append(pendingToolCalls, part.ToolCall)
					toolCalls[part.ToolCall.ID] = part.ToolCall
				}
			}
			continue
		}

		// Handle tool results - render tool call with result inline
		if msg.Role == llm.RoleTool {
			// First flush any pending text before tool results
			if pendingText.Len() > 0 {
				b.WriteString(pendingText.String())
				pendingText.Reset()
			}

			for _, part := range msg.Parts {
				if part.Type == llm.PartToolResult && part.ToolResult != nil {
					// Find the matching tool call
					tc := toolCalls[part.ToolResult.ID]
					if tc != nil {
						// Write tool call with result
						writeToolCall(&b, tc, part.ToolResult)
						delete(toolCalls, part.ToolResult.ID)
					} else {
						// Orphan tool result (shouldn't happen, but handle gracefully)
						b.WriteString("<details>\n")
						b.WriteString(fmt.Sprintf("<summary>%s (result)</summary>\n\n", part.ToolResult.Name))
						b.WriteString("**Result:**\n```\n")
						b.WriteString(part.ToolResult.Content)
						b.WriteString("\n```\n\n</details>\n\n")
					}
				}
			}
		}
	}

	// Flush any remaining assistant content
	flushAssistant()

	return b.String()
}

// writeToolCall writes a tool call with optional result in a collapsible block.
func writeToolCall(b *strings.Builder, tc *llm.ToolCall, result *llm.ToolResult) {
	b.WriteString("<details>\n")
	b.WriteString(fmt.Sprintf("<summary>%s</summary>\n\n", tc.Name))

	// Write arguments as JSON
	b.WriteString("```json\n")
	// Pretty print the JSON arguments
	var prettyArgs interface{}
	if err := json.Unmarshal(tc.Arguments, &prettyArgs); err == nil {
		if pretty, err := json.MarshalIndent(prettyArgs, "", "  "); err == nil {
			b.Write(pretty)
		} else {
			b.Write(tc.Arguments)
		}
	} else {
		b.Write(tc.Arguments)
	}
	b.WriteString("\n```\n\n")

	// Write result if provided
	if result != nil {
		if result.IsError {
			b.WriteString("**Error:**\n```\n")
		} else {
			b.WriteString("**Result:**\n```\n")
		}
		b.WriteString(result.Content)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("</details>\n\n")
}

// formatTokens formats input/output tokens in a readable format.
func formatTokens(input, output int) string {
	if input == 0 && output == 0 {
		return "-"
	}
	return fmt.Sprintf("%s in / %s out", formatCount(input), formatCount(output))
}

// formatCount formats a number in compact form (e.g., 1K, 1.2K, 3.4M).
func formatCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		val := float64(n) / 1000
		if val == float64(int(val)) {
			return fmt.Sprintf("%dK", int(val))
		}
		return fmt.Sprintf("%.1fK", val)
	}
	val := float64(n) / 1000000
	if val == float64(int(val)) {
		return fmt.Sprintf("%dM", int(val))
	}
	return fmt.Sprintf("%.1fM", val)
}
