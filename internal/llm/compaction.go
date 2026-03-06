package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const (
	defaultThresholdRatio        = 0.90
	defaultRecentUserTokenBudget = 20_000
	defaultMaxToolResultChars    = 80_000
	approxBytesPerToken          = 4

	// summarizationToolResultChars is the max chars of a tool result included in
	// the summarization prompt.  Enough to understand what the tool did without
	// flooding the compaction request with raw file/shell output.
	summarizationToolResultChars = 500
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
	if len(msgs) == 0 {
		return 0
	}
	total := 0
	for _, msg := range msgs {
		if len(msg.Parts) == 0 {
			continue
		}
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

const summarizationPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a detailed handoff summary for another instance of yourself that will resume this conversation.

Before your final summary, wrap your analysis in <analysis> tags. In your analysis:
1. Chronologically review each message and section of the conversation. For each section identify:
   - The user's explicit requests and intent
   - Key decisions made and actions taken
   - Specific details: file paths, variable names, error messages, API contracts, config values
   - Errors encountered and how they were resolved
   - Specific user feedback or corrections

Your summary must include all of the following sections:
1. Primary Request and Intent
2. Key Context (file paths, APIs, config values, constraints, environment details)
3. Actions Taken and Decisions Made
4. Errors and Fixes
5. All User Messages (verbatim — critical for understanding the user's exact intent and any changing instructions)
6. Current Work (precisely what was in-flight immediately before this summary)
7. Next Step (direct quote from the most recent conversation showing exactly what needs to happen next)

Wrap your final summary in <summary> tags.
Extract only the contents of the <summary> block as the result — the <analysis> is for your reasoning only.`

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

	originalCount := len(messages)
	// Keep the pre-sanitized slice for building the summarization text.
	// Sanitization converts orphaned tool calls to placeholder text that would
	// leak tool names into the summary; building from the original and skipping
	// PartToolCall parts avoids this.  Tool results capture the outcome, so
	// dropping the call side loses nothing useful for summarization.
	preSanitize := messages
	messages = sanitizeToolHistory(messages)

	// Build summarization request with the conversation history
	var sumReq []Message
	sumReq = append(sumReq, SystemText(summarizationPrompt))

	// Add a representation of the conversation
	var convText strings.Builder
	convText.WriteString("Here is the conversation to summarize:\n\n")
	for _, msg := range preSanitize {
		var lineParts []string
		for _, part := range msg.Parts {
			if part.Type == PartToolCall {
				// Skip tool calls — orphaned ones must not appear in the summary,
				// and matched ones are represented by their tool result below.
				continue
			}
			if part.Text != "" {
				lineParts = append(lineParts, part.Text)
			}
			if part.ToolResult != nil {
				content := TruncateToolResult(part.ToolResult.Content, summarizationToolResultChars)
				lineParts = append(lineParts, fmt.Sprintf("[tool_result: %s → %s]", part.ToolResult.Name, content))
			}
		}
		if len(lineParts) == 0 {
			continue
		}
		convText.WriteString(string(msg.Role))
		convText.WriteString(": ")
		for _, s := range lineParts {
			convText.WriteString(s)
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

	var userContent strings.Builder
	if systemPrompt != "" {
		userContent.WriteString("The system prompt for this conversation is:\n")
		userContent.WriteString(systemPrompt)
		userContent.WriteString("\n\n")
	}
	userContent.WriteString(convStr)
	userContent.WriteString("\n\nNow create the compaction summary.")
	sumReq = append(sumReq, UserText(userContent.String()))

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

	// Extract recent conversation tail within budget (user + assistant)
	recentMsgs := extractRecentContext(messages, config.RecentUserTokenBudget)

	// Reconstruct history
	newMessages := reconstructHistory(systemPrompt, summary.String(), recentMsgs)
	newMessages = sanitizeToolHistory(newMessages)

	return &CompactionResult{
		Summary:        summary.String(),
		NewMessages:    newMessages,
		OriginalCount:  originalCount,
		CompactedCount: len(newMessages),
	}, nil
}

// extractRecentContext walks messages newest→oldest, collecting both user and
// assistant messages until the token budget is exhausted. It cuts only at clean
// message boundaries and never splits a tool call / tool result pair.
// Returns the tail in chronological order.
func extractRecentContext(messages []Message, tokenBudget int) []Message {
	if len(messages) == 0 {
		return nil
	}

	// Walk backward, accumulating whole messages until budget is gone.
	// We stop before a message that would push us over, so the first message
	// collected is always the newest one that fits.
	remaining := tokenBudget
	cutIdx := len(messages) // exclusive lower bound (we'll slice messages[cutIdx:])

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		// Skip system messages — they're re-injected separately.
		if msg.Role == RoleSystem {
			continue
		}
		tokens := EstimateMessageTokens([]Message{msg})
		if tokens > remaining && cutIdx < len(messages) {
			// Budget exhausted; stop before this message.
			break
		}
		remaining -= tokens
		cutIdx = i
		if remaining <= 0 {
			break
		}
	}

	tail := messages[cutIdx:]

	// Ensure the tail forms a valid conversation: must start with a user message
	// (Anthropic and most providers reject histories that open with an assistant turn).
	for len(tail) > 0 && tail[0].Role != RoleUser {
		tail = tail[1:]
	}

	return tail
}

// reconstructHistory builds the compacted message list:
// [SystemText(systemPrompt)] + [summary(user, CacheAnchor)] + [ack(assistant)] + [recent context]
//
// The summary message is marked CacheAnchor=true so Anthropic-compatible providers
// apply cache_control: ephemeral to it, creating a stable cache breakpoint at the
// summary. This means subsequent turns only pay cold-prefill cost on the delta after
// the summary, not on the full compacted context.
func reconstructHistory(systemPrompt, summary string, recentMsgs []Message) []Message {
	var messages []Message

	if systemPrompt != "" {
		messages = append(messages, SystemText(systemPrompt))
	}

	// The summary message gets a cache anchor so the Anthropic provider applies
	// cache_control: ephemeral to it. The summary is stable until the next
	// compaction, so caching it here gives a cheap warm hit on every subsequent turn.
	messages = append(messages, Message{
		Role:        RoleUser,
		Parts:       []Part{{Type: PartText, Text: summaryPrefix + summary}},
		CacheAnchor: true,
	})

	// Add an assistant acknowledgement so the conversation flow is valid
	messages = append(messages, AssistantText("I've reviewed the context summary. I'll continue from where we left off."))

	messages = append(messages, recentMsgs...)

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
