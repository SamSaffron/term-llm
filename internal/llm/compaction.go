package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const (
	defaultThresholdRatio        = 0.80
	defaultRecentUserTokenBudget = 20_000
	defaultMaxToolResultChars    = 80_000
	approxBytesPerToken          = 4
)

// CompactionConfig controls when and how context compaction occurs.
type CompactionConfig struct {
	ThresholdRatio        float64 // Fraction of context window to trigger (default 0.80)
	RecentUserTokenBudget int     // Max tokens of recent user messages to keep
	MaxToolResultChars    int     // Max chars per tool result when recording
}

// DefaultCompactionConfig returns a CompactionConfig with sensible defaults.
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		ThresholdRatio:        defaultThresholdRatio,
		RecentUserTokenBudget: defaultRecentUserTokenBudget,
		MaxToolResultChars:    defaultMaxToolResultChars,
	}
}

// CompactionResult describes what happened during compaction.
type CompactionResult struct {
	Summary        string
	NewMessages    []Message
	OriginalCount  int
	CompactedCount int
}

// EstimateTokens returns an approximate token count for a string using a
// simple 4-bytes-per-token heuristic (same as codex).
func EstimateTokens(text string) int {
	return (len(text) + approxBytesPerToken - 1) / approxBytesPerToken
}

// EstimateMessageTokens returns an approximate token count for a slice of
// messages by summing all text content across parts.
func EstimateMessageTokens(msgs []Message) int {
	total := 0
	for _, msg := range msgs {
		for _, part := range msg.Parts {
			total += EstimateTokens(part.Text)
			if part.ToolCall != nil {
				total += EstimateTokens(string(part.ToolCall.Arguments))
			}
			if part.ToolResult != nil {
				total += EstimateTokens(part.ToolResult.Content)
			}
		}
	}
	return total
}

const summarizationPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another instance of yourself that will resume this conversation.

Include:
- The overall goal/task being worked on
- What has been accomplished so far (key decisions, actions taken)
- Important context (file paths, variable names, error messages, constraints)
- What remains to be done (clear next steps)

Be concise and structured. Focus on details that would be lost without this summary.`

const summaryPrefix = `[Context Compaction]
A previous conversation was compacted to fit within the context window. Below is a summary of what happened before. Use this context to continue seamlessly.

Summary:
`

// Compact generates a summary of the conversation history and returns a
// compacted message list: [system] + [summary as user] + [recent user messages].
func Compact(ctx context.Context, provider Provider, model, systemPrompt string, messages []Message, config CompactionConfig) (*CompactionResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to compact")
	}

	// Build summarization request with the conversation history
	var sumReq []Message
	sumReq = append(sumReq, SystemText(summarizationPrompt))
	if systemPrompt != "" {
		sumReq = append(sumReq, UserText("The system prompt for this conversation is:\n"+systemPrompt))
	}

	// Add a representation of the conversation
	var convText strings.Builder
	convText.WriteString("Here is the conversation to summarize:\n\n")
	for _, msg := range messages {
		convText.WriteString(string(msg.Role))
		convText.WriteString(": ")
		for _, part := range msg.Parts {
			if part.Text != "" {
				convText.WriteString(part.Text)
			}
			if part.ToolCall != nil {
				convText.WriteString(fmt.Sprintf("[tool_call: %s(%s)]", part.ToolCall.Name, string(part.ToolCall.Arguments)))
			}
			if part.ToolResult != nil {
				content := part.ToolResult.Content
				if len(content) > 2000 {
					content = content[:1000] + "\n...[truncated]...\n" + content[len(content)-1000:]
				}
				convText.WriteString(fmt.Sprintf("[tool_result: %s → %s]", part.ToolResult.Name, content))
			}
		}
		convText.WriteString("\n\n")
	}
	// Truncate conversation text if it's too large for the summarization request.
	// Use ~75% of the input limit (if known) to leave room for the summary output
	// and framing messages. Fall back to 400K chars (~100K tokens) if unknown.
	convStr := convText.String()
	maxConvChars := 400_000
	if inputLimit := InputLimitForModel(model); inputLimit > 0 {
		maxConvChars = inputLimit * approxBytesPerToken * 3 / 4
	}
	convRunes := []rune(convStr)
	if len(convRunes) > maxConvChars {
		half := maxConvChars / 2
		convStr = string(convRunes[:half]) + "\n...[conversation truncated for summarization]...\n" + string(convRunes[len(convRunes)-half:])
	}

	sumReq = append(sumReq, UserText(convStr))
	sumReq = append(sumReq, UserText("Now create the compaction summary."))

	// Call provider with no tools (pure text completion)
	stream, err := provider.Stream(ctx, Request{
		Model:    model,
		Messages: sumReq,
		// No tools - pure text completion
	})
	if err != nil {
		return nil, fmt.Errorf("compaction stream failed: %w", err)
	}
	defer stream.Close()

	// Collect summary text
	var summary strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("compaction recv failed: %w", err)
		}
		if event.Type == EventTextDelta {
			summary.WriteString(event.Text)
		}
	}

	if summary.Len() == 0 {
		return nil, fmt.Errorf("compaction produced empty summary")
	}

	// Extract recent user messages within budget
	recentUserMsgs := extractRecentUserMessages(messages, config.RecentUserTokenBudget)

	// Reconstruct history
	newMessages := reconstructHistory(systemPrompt, summary.String(), recentUserMsgs)

	return &CompactionResult{
		Summary:        summary.String(),
		NewMessages:    newMessages,
		OriginalCount:  len(messages),
		CompactedCount: len(newMessages),
	}, nil
}

// extractRecentUserMessages walks messages newest→oldest, collecting user-role
// messages until the token budget is exhausted. Returns in chronological order.
func extractRecentUserMessages(messages []Message, tokenBudget int) []Message {
	var result []Message
	remaining := tokenBudget

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != RoleUser {
			continue
		}
		tokens := EstimateMessageTokens([]Message{msg})
		if tokens > remaining && len(result) > 0 {
			break
		}
		remaining -= tokens
		result = append([]Message{msg}, result...)
		if remaining <= 0 {
			break
		}
	}

	return result
}

// reconstructHistory builds the compacted message list:
// [SystemText(systemPrompt)] + [UserText(summaryPrefix + summary)] + [recent user messages]
func reconstructHistory(systemPrompt, summary string, recentUserMsgs []Message) []Message {
	var messages []Message

	if systemPrompt != "" {
		messages = append(messages, SystemText(systemPrompt))
	}

	messages = append(messages, UserText(summaryPrefix+summary))

	// Add an assistant acknowledgement so the conversation flow is valid
	messages = append(messages, AssistantText("I've reviewed the context summary. I'll continue from where we left off."))

	messages = append(messages, recentUserMsgs...)

	return messages
}

// TruncateToolResult preserves the first half and last half of long tool
// results, inserting a truncation marker in the middle.
// Uses rune count to avoid splitting multi-byte UTF-8 characters.
func TruncateToolResult(content string, maxChars int) string {
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content
	}

	head := maxChars / 2
	tail := maxChars - head
	truncated := len(runes) - maxChars
	// Count lines in the truncated middle section to give the LLM more context
	middle := string(runes[head : len(runes)-tail])
	lines := 1 + strings.Count(middle, "\n")
	return string(runes[:head]) + fmt.Sprintf("\n[...%d chars truncated - %d lines...]\n", truncated, lines) + string(runes[len(runes)-tail:])
}

// isContextOverflowError checks whether an error indicates that the context
// window was exceeded. Checks error strings across providers.
func isContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	patterns := []string{
		"context length exceeded",
		"maximum context length",
		"context_length_exceeded",
		"too many tokens",
		"request too large",
		"prompt is too long",
		"input is too long",
		"content too large",
		"token limit",
		"exceeds the model's maximum context",
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}
