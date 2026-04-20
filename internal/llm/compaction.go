package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const (
	defaultThresholdRatio     = 0.90
	defaultMaxToolResultChars = 80_000
	defaultSummaryTokenBudget = 10_000
	approxBytesPerToken       = 4
)

// CompactionConfig controls when and how context compaction occurs.
type CompactionConfig struct {
	ThresholdRatio     float64 // Fraction of context window to trigger (default 0.80)
	MaxToolResultChars int     // Max chars per tool result when recording
	SummaryTokenBudget int     // Max output tokens for the compaction summary
	InputLimit         int     // Provider-effective input token limit (0 = use canonical)
}

// DefaultCompactionConfig returns a CompactionConfig with sensible defaults.
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		ThresholdRatio:     defaultThresholdRatio,
		MaxToolResultChars: defaultMaxToolResultChars,
		SummaryTokenBudget: defaultSummaryTokenBudget,
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

const compactionPrompt = `Create a detailed summary of our conversation so far. This summary will replace the conversation history, so include everything needed to continue seamlessly.

Your summary must cover:
1. The user's primary goal and any evolving intent
2. Key context: file paths, APIs, config values, error messages, environment details
3. Decisions made and actions taken (chronologically)
4. Current state: what was just completed and what is in progress
5. Exact next steps needed

Be specific and concrete — include exact file paths, function names, error messages, and code snippets when relevant. Omit small talk and pleasantries.

Budget: keep your summary under 2500 words.`

const summaryPrefix = `[Context Compaction]
A previous conversation was compacted to fit within the context window. Below is a summary of what happened before. Use this context to continue seamlessly.

Summary:
`

// isolatedConversationProvider returns a provider instance that can service
// helper requests like compaction/handover without mutating the live
// provider-side conversation state.
func isolatedConversationProvider(provider Provider) Provider {
	switch p := provider.(type) {
	case *RetryProvider:
		return &RetryProvider{inner: isolatedConversationProvider(p.inner), config: p.config}
	case *OpenAIProvider:
		clone := *p
		clone.responsesClient = cloneResponsesClientFreshConversation(p.responsesClient)
		return &clone
	case *ChatGPTProvider:
		clone := *p
		clone.responsesClient = cloneResponsesClientFreshConversation(p.responsesClient)
		return &clone
	case *CopilotProvider:
		clone := *p
		clone.responsesClient = cloneResponsesClientFreshConversation(p.responsesClient)
		return &clone
	default:
		return provider
	}
}

// Compact generates a summary of the conversation history and returns a
// compacted message list: [system] + [summary as user] + [recent user messages].
//
// Instead of serializing the conversation to text, it appends the compaction
// instruction to the existing messages — leveraging prompt cache on providers
// like Anthropic — and enforces a token budget on the output.
func Compact(ctx context.Context, provider Provider, model, systemPrompt string, messages []Message, config CompactionConfig) (*CompactionResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to compact")
	}

	originalCount := len(messages)
	messages = sanitizeToolHistory(messages)

	// Build request: [system] + sanitized messages + compaction instruction.
	var reqMessages []Message
	if systemPrompt != "" {
		reqMessages = append(reqMessages, SystemText(systemPrompt))
	}
	reqMessages = append(reqMessages, messages...)

	// If messages exceed the input limit, trim from the front (after system)
	// to fit. This handles reactive compaction where we're already at or past
	// the context window. Keep ~75% of the input limit for conversation
	// messages, reserving the rest for the output budget and framing.
	// Use provider-effective limit if set, else fall back to canonical.
	inputLimit := config.InputLimit
	if inputLimit <= 0 {
		inputLimit = InputLimitForModel(model)
	}
	if inputLimit > 0 {
		maxInputTokens := inputLimit * 3 / 4
		reqMessages = trimMessagesToFit(reqMessages, maxInputTokens)
	}

	// Ensure valid role alternation: if the last message is user or tool,
	// insert a minimal assistant message before the compaction user message.
	if len(reqMessages) > 0 {
		lastRole := reqMessages[len(reqMessages)-1].Role
		if lastRole == RoleUser || lastRole == RoleTool {
			reqMessages = append(reqMessages, AssistantText("I'll now summarize our conversation."))
		}
	}
	reqMessages = append(reqMessages, UserText(compactionPrompt))

	budget := config.SummaryTokenBudget
	if budget <= 0 {
		budget = defaultSummaryTokenBudget
	}
	// Centralized output clamping: cap to model's max output limit so providers
	// with small output limits don't reject the request. Providers also clamp
	// individually, but doing it here provides belt-and-suspenders safety.
	budget = ClampOutputTokens(budget, model)

	// Call provider with no tools, enforcing output budget. Use an isolated
	// provider instance so the helper turn doesn't overwrite live server-side
	// conversation state (for example previous_response_id on Responses API clients).
	stream, err := isolatedConversationProvider(provider).Stream(ctx, Request{
		Model:           model,
		Messages:        reqMessages,
		MaxOutputTokens: budget,
	})
	if err != nil {
		return nil, fmt.Errorf("compaction stream failed: %w", err)
	}
	defer stream.Close()

	// Collect summary text — entire output is the summary.
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

	// Reconstruct history: system + summary + ack. No recent turns are carried
	// forward — the summary captures everything needed. Appending raw recent
	// turns would bloat context and break thinking models (tool call/result
	// pairs can't be injected into a compacted user-message history).
	newMessages := reconstructHistory(systemPrompt, summary.String(), nil)

	return &CompactionResult{
		Summary:        summary.String(),
		NewMessages:    newMessages,
		OriginalCount:  originalCount,
		CompactedCount: len(newMessages),
	}, nil
}

// HandoverResult describes the outcome of a handover compression.
type HandoverResult struct {
	Document    string    // The handover document text
	NewMessages []Message // [system] + [handover doc as user] + [assistant ack]
	SourceAgent string
	TargetAgent string
}

const handoverPromptTemplate = `You are handing over this conversation to a different agent (%s -> %s). Create a structured handover briefing that the new agent can use to continue the work.

Your handover document must include:
1. **Objective** — what the user is trying to accomplish
2. **Work Completed** — what was explored, discussed, and decided (chronologically)
3. **Current State** — exact files involved, errors found, test results, what's in progress
4. **Pending Tasks** — what still needs to be done, in priority order
5. **Key Context** — file paths, function names, config values, constraints, user preferences

Be specific and concrete — include exact file paths, function names, error messages, and code snippets when relevant. The new agent has no other context beyond this document.

Budget: keep your briefing under 3000 words.`

// handoverPrefix is prepended to the handover document in the reconstructed message history.
func handoverPrefix(source, target string) string {
	return fmt.Sprintf("[Agent Handover: @%s -> @%s]\n\n", source, target)
}

// Handover generates a handover document from the conversation history using
// the outgoing provider. This is the Tier 2 fallback used when file-based
// handover is not available. The result contains reconstructed messages suitable
// for the new agent: [new system prompt] + [handover doc] + [assistant ack].
func Handover(ctx context.Context, provider Provider, model, currentSystemPrompt, newSystemPrompt string, messages []Message, sourceAgent, targetAgent string, config CompactionConfig) (*HandoverResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to hand over")
	}

	messages = sanitizeToolHistory(messages)

	// Build request: [system] + sanitized messages + handover instruction.
	var reqMessages []Message
	if currentSystemPrompt != "" {
		reqMessages = append(reqMessages, SystemText(currentSystemPrompt))
	}
	reqMessages = append(reqMessages, messages...)

	// Trim to fit input limits (same logic as Compact).
	inputLimit := config.InputLimit
	if inputLimit <= 0 {
		inputLimit = InputLimitForModel(model)
	}
	if inputLimit > 0 {
		maxInputTokens := inputLimit * 3 / 4
		reqMessages = trimMessagesToFit(reqMessages, maxInputTokens)
	}

	// Ensure valid role alternation before appending the handover prompt.
	if len(reqMessages) > 0 {
		lastRole := reqMessages[len(reqMessages)-1].Role
		if lastRole == RoleUser || lastRole == RoleTool {
			reqMessages = append(reqMessages, AssistantText("I'll now prepare the handover briefing."))
		}
	}

	prompt := fmt.Sprintf(handoverPromptTemplate, sourceAgent, targetAgent)
	reqMessages = append(reqMessages, UserText(prompt))

	budget := 12_000 // Slightly larger budget than compaction for handover precision
	budget = ClampOutputTokens(budget, model)

	stream, err := isolatedConversationProvider(provider).Stream(ctx, Request{
		Model:           model,
		Messages:        reqMessages,
		MaxOutputTokens: budget,
	})
	if err != nil {
		return nil, fmt.Errorf("handover stream failed: %w", err)
	}
	defer stream.Close()

	var document strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("handover recv failed: %w", err)
		}
		if event.Type == EventTextDelta {
			document.WriteString(event.Text)
		}
	}

	if document.Len() == 0 {
		return nil, fmt.Errorf("handover produced empty document")
	}

	// Reconstruct messages for the new agent with the new system prompt.
	newMessages := ReconstructHandoverHistory(newSystemPrompt, document.String(), sourceAgent, targetAgent)

	return &HandoverResult{
		Document:    document.String(),
		NewMessages: newMessages,
		SourceAgent: sourceAgent,
		TargetAgent: targetAgent,
	}, nil
}

// HandoverFromFile creates a HandoverResult from an existing handover document
// file (Tier 1: zero LLM cost). Used when the outgoing agent has enable_handover
// set and has written content to the handover directory.
func HandoverFromFile(content, newSystemPrompt, sourceAgent, targetAgent string) *HandoverResult {
	newMessages := ReconstructHandoverHistory(newSystemPrompt, content, sourceAgent, targetAgent)
	return &HandoverResult{
		Document:    content,
		NewMessages: newMessages,
		SourceAgent: sourceAgent,
		TargetAgent: targetAgent,
	}
}

// ReconstructHandoverHistory builds the message list for the new agent:
// [SystemText(newSystemPrompt)] + [handover doc (user, CacheAnchor)] + [assistant ack]
func ReconstructHandoverHistory(systemPrompt, document, sourceAgent, targetAgent string) []Message {
	var messages []Message

	if systemPrompt != "" {
		messages = append(messages, SystemText(systemPrompt))
	}

	prefix := handoverPrefix(sourceAgent, targetAgent)
	messages = append(messages, Message{
		Role:        RoleUser,
		Parts:       []Part{{Type: PartText, Text: prefix + document}},
		CacheAnchor: true,
	})

	ack := fmt.Sprintf("I've reviewed the handover briefing from @%s. I'll continue from where they left off.", sourceAgent)
	messages = append(messages, AssistantText(ack))

	return messages
}

// trimMessagesToFit removes messages from the front (after any system message)
// until the total estimated tokens fit within maxTokens. Preserves the system
// message, cache-anchored messages (previous compaction summaries), and the
// most recent messages. Ensures the result starts with a system or user message
// (not assistant/tool). Falls back to truncating oversized single messages.
func trimMessagesToFit(messages []Message, maxTokens int) []Message {
	if EstimateMessageTokens(messages) <= maxTokens {
		return messages
	}

	// Separate system prefix from conversation messages.
	startIdx := 0
	if len(messages) > 0 && messages[0].Role == RoleSystem {
		startIdx = 1
	}

	// Check for cache-anchored block after system (previous compaction summary
	// and its assistant ack). Preserving this during re-compaction retains
	// context from earlier compaction rounds.
	anchorEnd := startIdx
	if anchorEnd < len(messages) && messages[anchorEnd].CacheAnchor {
		anchorEnd++
		// Include the following assistant ack to maintain valid role alternation.
		if anchorEnd < len(messages) && messages[anchorEnd].Role == RoleAssistant {
			anchorEnd++
		}
	}

	// First try: preserve anchor block if present.
	if anchorEnd > startIdx {
		if result := doTrim(messages, anchorEnd, maxTokens); EstimateMessageTokens(result) <= maxTokens {
			return result
		}
	}

	// Fallback: trim from after system, dropping anchor if needed.
	return doTrim(messages, startIdx, maxTokens)
}

// doTrim preserves messages[:preserveEnd] and drops from the front of the
// remainder until the total fits within maxTokens. Ensures the trimmable
// portion starts with a user message. Truncates oversized single messages.
func doTrim(messages []Message, preserveEnd, maxTokens int) []Message {
	prefix := messages[:preserveEnd]
	conv := messages[preserveEnd:]
	for len(conv) > 1 && EstimateMessageTokens(prefix)+EstimateMessageTokens(conv) > maxTokens {
		conv = conv[1:]
	}

	// If a single message exceeds budget, truncate its text/tool content.
	if len(conv) == 1 {
		prefixTokens := EstimateMessageTokens(prefix)
		if prefixTokens+EstimateMessageTokens(conv) > maxTokens {
			remaining := maxTokens - prefixTokens
			if remaining > 0 {
				maxChars := remaining * approxBytesPerToken
				conv = []Message{truncateMessageParts(conv[0], maxChars)}
			}
		}
	}

	// Ensure we start with a user message (providers reject leading assistant/tool).
	for len(conv) > 0 && conv[0].Role != RoleUser {
		conv = conv[1:]
	}

	result := make([]Message, 0, preserveEnd+len(conv))
	result = append(result, prefix...)
	result = append(result, conv...)
	return result
}

// truncateMessageParts truncates text and tool result content in a message
// to fit within maxChars total. Used when a single oversized message exceeds
// the compaction input budget.
func truncateMessageParts(msg Message, maxChars int) Message {
	result := Message{
		Role:        msg.Role,
		CacheAnchor: msg.CacheAnchor,
		Parts:       make([]Part, len(msg.Parts)),
	}
	copy(result.Parts, msg.Parts)
	for i, part := range result.Parts {
		if part.Text != "" {
			result.Parts[i].Text = TruncateToolResult(part.Text, maxChars)
		}
		if part.ToolResult != nil && len(part.ToolResult.Content) > maxChars {
			tr := *part.ToolResult
			tr.Content = TruncateToolResult(tr.Content, maxChars)
			result.Parts[i].ToolResult = &tr
		}
	}
	return result
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
