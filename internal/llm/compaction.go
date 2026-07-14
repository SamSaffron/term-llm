package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	defaultThresholdRatio     = 0.90
	defaultSoftThresholdRatio = 0.90
	defaultHardThresholdRatio = 0.95
	defaultMaxToolResultChars = 80_000
	defaultSummaryTokenBudget = 10_000
	defaultRecentRawTurns     = 2
	maxRecentRawTokenBudget   = 8_000
	minRecentRawTokenBudget   = 1_000
	approxBytesPerToken       = 4
	maxPreviousTurnsChars     = 30_000
)

// CompactionConfig controls when and how context compaction occurs.
type CompactionConfig struct {
	ThresholdRatio       float64 // Legacy/default fraction of context window to trigger compaction (default 0.90)
	SoftThresholdRatio   float64 // Fraction where we try to checkpoint and compact cleanly (default 0.90)
	HardThresholdRatio   float64 // Fraction where we must compact before the next tool/LLM continuation (default 0.95)
	MaxToolResultChars   int     // Max chars per tool result when recording
	SummaryTokenBudget   int     // Max output tokens for the compaction summary
	RecentRawTokenBudget int     // Max tokens of recent raw transcript to carry after compaction (0 = auto, <0 = disabled)
	RecentRawTurns       int     // Max recent user turns to try preserving raw (0 = default, <0 = disabled)
	InputLimit           int     // Provider-effective input token limit (0 = use canonical)
}

// DefaultCompactionConfig returns a CompactionConfig with sensible defaults.
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		ThresholdRatio:       defaultThresholdRatio,
		SoftThresholdRatio:   defaultSoftThresholdRatio,
		HardThresholdRatio:   defaultHardThresholdRatio,
		MaxToolResultChars:   defaultMaxToolResultChars,
		SummaryTokenBudget:   defaultSummaryTokenBudget,
		RecentRawTokenBudget: 0, // auto-size from provider input window, capped at 8k
		RecentRawTurns:       defaultRecentRawTurns,
	}
}

func effectiveCompactionThresholdRatios(config *CompactionConfig) (soft, hard float64) {
	soft, hard = defaultSoftThresholdRatio, defaultHardThresholdRatio
	if config == nil {
		return soft, hard
	}
	if config.SoftThresholdRatio > 0 {
		soft = config.SoftThresholdRatio
	} else if config.ThresholdRatio > 0 {
		soft = config.ThresholdRatio
	}
	if config.HardThresholdRatio > 0 {
		hard = config.HardThresholdRatio
	} else if config.ThresholdRatio > 0 {
		hard = config.ThresholdRatio
	}
	return soft, hard
}

// CompactionResult describes what happened during compaction.
type CompactionResult struct {
	Summary        string
	NewMessages    []Message
	OriginalCount  int
	CompactedCount int
	Model          string // Model used by the helper LLM call.
	Usage          Usage  // Token usage/cost of the helper LLM call that produced the summary.
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
		total += estimateSingleMessageTokens(msg)
	}
	return total
}

func estimateSingleMessageTokens(msg Message) int {
	if msg.Role == RoleEvent || len(msg.Parts) == 0 {
		return 0
	}
	total := 0
	for _, part := range msg.Parts {
		total += EstimateTokens(part.Text)
		total += EstimateTokens(part.ReasoningContent)
		for _, summaryPart := range part.ReasoningSummaryParts {
			total += EstimateTokens(summaryPart)
		}
		total += EstimateTokens(part.ReasoningEncryptedContent)
		if part.ToolCall != nil {
			total += EstimateTokens(string(part.ToolCall.Arguments))
		}
		if part.ToolResult != nil {
			total += EstimateTokens(part.ToolResult.Content)
		}
	}
	return total
}

// preparedCompactionContext is the split used by both hard and soft
// compaction: summarize the old prefix, then replay a bounded recent suffix as
// true structured messages after the summary. Keeping the split in one place
// prevents hard/soft modes from drifting and avoids duplicating recent context
// inside the extractive <PREVIOUS_TURNS> block.
type preparedCompactionContext struct {
	SummaryMessages []Message
	RecentMessages  []Message
}

func prepareCompactionContext(messages []Message, config CompactionConfig, skipBrief string) preparedCompactionContext {
	source := filterCompactionControlMessages(messages, skipBrief)
	if len(source) == 0 {
		return preparedCompactionContext{}
	}

	budget := effectiveRecentRawTokenBudget(config)
	turns := effectiveRecentRawTurns(config)
	if budget <= 0 || turns <= 0 || len(source) <= 1 {
		return preparedCompactionContext{SummaryMessages: source}
	}

	start := selectRecentRawSuffixStart(source, budget, turns)
	if start <= 0 || start >= len(source) {
		return preparedCompactionContext{SummaryMessages: source}
	}

	summaryMessages := source[:start]
	if !hasCompactionSummaryInput(summaryMessages) {
		// If the split would leave nothing meaningful to summarize, keep the old
		// summary-only behavior. This avoids compacting tiny one-turn histories into
		// summary+duplicate raw messages.
		return preparedCompactionContext{SummaryMessages: source}
	}

	recent := sanitizeRecentRawSuffix(source[start:])
	if len(recent) == 0 {
		return preparedCompactionContext{SummaryMessages: source}
	}
	return preparedCompactionContext{SummaryMessages: summaryMessages, RecentMessages: recent}
}

func filterCompactionControlMessages(messages []Message, skipBrief string) []Message {
	brief := strings.TrimSpace(skipBrief)
	filtered := FilterConversationMessages(messages)
	skipBriefIndex := -1
	if brief != "" {
		for i := len(filtered) - 1; i >= 0; i-- {
			msg := filtered[i]
			if msg.Role == RoleAssistant && strings.TrimSpace(continuationBriefFromAssistantMessage(msg)) == brief {
				skipBriefIndex = i
				break
			}
		}
	}
	out := make([]Message, 0, len(filtered))
	for i, msg := range filtered {
		if msg.Role == RoleSystem || msg.Role == RoleDeveloper || msg.Role == RoleEvent {
			continue
		}
		if isCompactionControlPromptText(MessageText(msg)) {
			continue
		}
		if i == skipBriefIndex {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func isCompactionControlPromptText(text string) bool {
	text = strings.TrimSpace(text)
	return text == strings.TrimSpace(contextContinuationBriefPrompt) || text == strings.TrimSpace(contextContinuationPrompt)
}

func effectiveRecentRawTurns(config CompactionConfig) int {
	if config.RecentRawTurns < 0 {
		return 0
	}
	if config.RecentRawTurns == 0 {
		return defaultRecentRawTurns
	}
	return config.RecentRawTurns
}

func effectiveRecentRawTokenBudget(config CompactionConfig) int {
	if config.RecentRawTokenBudget < 0 {
		return 0
	}
	if config.RecentRawTokenBudget > 0 {
		return config.RecentRawTokenBudget
	}
	if config.InputLimit <= 0 {
		return maxRecentRawTokenBudget
	}

	// Auto mode keeps the exact raw suffix useful but intentionally small: about
	// 5% of the provider-effective input window, bounded so compaction artifacts
	// do not consume too much of the freshly compacted context.
	budget := config.InputLimit / 20
	if budget > maxRecentRawTokenBudget {
		budget = maxRecentRawTokenBudget
	}
	if budget < minRecentRawTokenBudget {
		budget = minRecentRawTokenBudget
	}
	return budget
}

func selectRecentRawSuffixStart(messages []Message, budget, maxTurns int) int {
	turnStarts := realUserTurnStarts(messages)
	if len(turnStarts) == 0 {
		return -1
	}

	suffixTokens := make([]int, len(messages)+1)
	for i := len(messages) - 1; i >= 0; i-- {
		suffixTokens[i] = suffixTokens[i+1] + estimateSingleMessageTokens(messages[i])
	}
	tryStart := func(start int) bool {
		return start > 0 && start < len(messages) && suffixTokens[start] <= budget
	}

	turnCount := min(maxTurns, len(turnStarts))
	for count := turnCount; count >= 1; count-- {
		start := turnStarts[len(turnStarts)-count]
		if tryStart(start) {
			return start
		}
	}
	latestTurnStart := turnStarts[len(turnStarts)-1]

	// Split an oversized latest turn. Only start at role-safe boundaries; never
	// start with a tool result because that would orphan it from its tool call.
	for i := latestTurnStart + 1; i < len(messages); i++ {
		if !isValidRecentRawStart(messages[i]) {
			continue
		}
		if EstimateMessageTokens(messages[i:]) <= budget {
			return i
		}
	}
	return -1
}

func realUserTurnStarts(messages []Message) []int {
	starts := make([]int, 0, len(messages))
	for i, msg := range messages {
		if msg.Role == RoleUser && !isInternalCompactionSummaryMessage(msg) && strings.TrimSpace(MessageText(msg)) != "" {
			starts = append(starts, i)
		}
	}
	return starts
}

func isValidRecentRawStart(msg Message) bool {
	switch msg.Role {
	case RoleUser:
		return !isInternalCompactionSummaryMessage(msg)
	case RoleAssistant:
		return true
	default:
		return false
	}
}

func sanitizeRecentRawSuffix(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == RoleSystem || msg.Role == RoleDeveloper || msg.Role == RoleEvent {
			continue
		}
		parts := cloneParts(msg.Parts)
		if len(parts) == 0 {
			continue
		}
		copyMsg := Message{Role: msg.Role, Parts: parts}
		// The compacted summary is the cache anchor. Do not carry any stale anchor
		// from a retained raw suffix into the reconstructed context.
		copyMsg.CacheAnchor = false
		cloned = append(cloned, copyMsg)
	}
	return dropEmptyMessages(sanitizeToolHistory(cloned))
}

func dropEmptyMessages(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if len(msg.Parts) == 0 {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func hasCompactionSummaryInput(messages []Message) bool {
	for _, msg := range messages {
		if msg.Role == RoleSystem || msg.Role == RoleDeveloper || msg.Role == RoleEvent {
			continue
		}
		if strings.TrimSpace(MessageText(msg)) != "" || messageHasToolMetadata(msg) {
			return true
		}
	}
	return false
}

func prepareMessagesForSummaryHelper(messages []Message) []Message {
	messages = sanitizeToolHistory(messages)
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		copyMsg := Message{Role: msg.Role, CacheAnchor: msg.CacheAnchor, Parts: cloneParts(msg.Parts)}
		for i := range copyMsg.Parts {
			part := &copyMsg.Parts[i]
			if part.Type == PartText || part.Type == PartFile {
				if summary, ok := compactionSummaryTextForStaticInfo(part.Text); ok {
					part.Text = summaryPrefix + summary
				}
			}
			part.ReasoningContent = ""
			part.ReasoningSummaryParts = nil
			part.ReasoningItemID = ""
			part.ReasoningEncryptedContent = ""
			part.ReasoningKind = ""
			part.ReasoningSummaryTitle = ""
		}
		out = append(out, copyMsg)
	}
	return out
}

const structuredCompactionBriefInstructions = `This is automatic internal compaction, not a user stop/cancel/wait request or a user summarize request. Do not infer that the user asked to stop, wait, summarize, avoid tools, or change direction unless a real user message explicitly says so.

If the provided history contains an earlier [Context Compaction] summary, treat it as the prior anchored summary. Preserve still-true details, remove stale details, and merge in new facts from the newer history.

Return exactly this Markdown structure. Keep every heading, section, and order. Use concise bullets instead of prose paragraphs. Use "(none)" when a section has no useful content. Do not include <PREVIOUS_TURNS> or <SUMMARY_AND_NEXT_ACTIONS> tags; the system adds those.

## Objective
- Current objective:
- Latest real user instruction:

## Constraints & Preferences
- User preferences:
- Developer/system constraints:
- Style/implementation constraints:

## Current State
- Complete:
- In progress:
- Worktree/session state:
- Passing checks:
- Failing checks:

## Important Context
- Durable facts:
- Key decisions:
- Assumptions:
- Tried and failed:

## Relevant Files & Symbols
- path or path:symbol: why it matters

## Tool/Test Outcomes
- Commands/tools run:
- Important outcomes:

## Recent Interaction Notes
- User feedback:
- Assistant actions:
- Retained raw suffix notes:

## Next Actions
1. Exact next action:
2. Then:
3. Then:

## Open Questions / Blockers
- Questions:
- Blockers:

## Control State
- Continue automatically by default.
- Ask the user only if blocked or if the latest real user instruction requires input.
- Wait only on explicit user stop/wait or blocked user input.
- Do not treat this compaction as a stop/cancel/wait/summarize request.

Rules:
- Preserve exact file paths, function names, type names, config keys, commands, error messages, test names, URLs, and user-stated preferences when known.
- If something is uncertain, write "uncertain:" and explain briefly.
- Do not reproduce raw tool transcripts, large file contents, assistant reasoning text, or hidden chain-of-thought.
- If recent raw messages are retained separately, treat them as authoritative and do not duplicate them unless required for continuity.
- Do not call tools. Do not answer the user's task directly. Do not claim the task is complete unless it actually is.
- Omit pleasantries and meta-commentary; return only the Markdown brief.
- Target 800-1600 words when there is enough context. Absolute maximum: 2500 words.`

const compactionPrompt = `Create a compact continuation brief for the conversation history you are given. This brief will replace older context. Newer messages may be retained separately as exact raw structured context after the brief, so focus on durable state and avoid duplicating recent raw context unless it is needed for continuity.

` + structuredCompactionBriefInstructions

const summaryPrefix = `[Context Compaction]
Internal context only; not a user command, stop/cancel/wait request. Continue from the latest real user instruction.

`

const contextContinuationBriefPrompt = `Context budget is getting tight. Create a compact continuation brief for the conversation history you are given. Newer messages may be retained separately as exact raw structured context after the brief, so focus on durable state and avoid duplicating recent raw context unless it is needed for continuity.

` + structuredCompactionBriefInstructions

const (
	previousTurnsOpen  = "<PREVIOUS_TURNS>\n"
	previousTurnsClose = "\n</PREVIOUS_TURNS>"
	summaryOpen        = "<SUMMARY_AND_NEXT_ACTIONS>\n"
	summaryClose       = "\n</SUMMARY_AND_NEXT_ACTIONS>"
)

// BuildCompactionStaticInfo returns a deterministic, bounded <PREVIOUS_TURNS>
// block of high-signal context to supplement an LLM-written continuation
// summary. The block is capped at 30k chars or 5% of the provider input window
// converted to chars with approxBytesPerToken, whichever is smaller. If
// inputLimit is unknown, the 30k char cap is used.
func BuildCompactionStaticInfo(messages []Message, inputLimit int) string {
	return buildCompactionStaticInfo(messages, inputLimit, "")
}

// CompactionResultFromBrief builds a normal CompactionResult from a
// continuation brief that has already been produced by an LLM. It intentionally
// avoids a second summary-helper LLM call: a deterministic <PREVIOUS_TURNS>
// block from the summarized prefix followed by the LLM's Summary / Next Actions
// becomes the compacted summary, then a bounded recent raw suffix is replayed as
// true structured messages.
func CompactionResultFromBrief(systemPrompt, brief string, messages []Message, config CompactionConfig) *CompactionResult {
	brief = strings.TrimSpace(brief)
	prepared := prepareCompactionContext(messages, config, brief)
	return compactionResultFromBriefPrepared(systemPrompt, brief, prepared, len(messages), config)
}

func compactionResultFromBriefPrepared(systemPrompt, brief string, prepared preparedCompactionContext, originalCount int, config CompactionConfig) *CompactionResult {
	brief = strings.TrimSpace(brief)
	previousTurns := buildCompactionStaticInfo(prepared.SummaryMessages, config.InputLimit, brief)
	var combined strings.Builder
	if strings.TrimSpace(previousTurns) != "" {
		combined.WriteString(previousTurns)
		combined.WriteString("\n\n")
	}
	combined.WriteString(summaryOpen)
	combined.WriteString(brief)
	combined.WriteString(summaryClose)
	summary := strings.TrimRight(combined.String(), "\n")
	newMessages := reconstructHistory(systemPrompt, summary, prepared.RecentMessages)
	return &CompactionResult{
		Summary:        summary,
		NewMessages:    newMessages,
		OriginalCount:  originalCount,
		CompactedCount: len(newMessages),
	}
}

// SoftCompact performs the one LLM call needed for manual soft compaction. The
// helper asks for a continuation brief using an isolated provider conversation,
// records that helper-call usage, and then deterministically reconstructs the
// compacted context from a <PREVIOUS_TURNS> block, the brief, and any bounded
// raw suffix selected by the shared compaction split.
func SoftCompact(ctx context.Context, provider Provider, model, systemPrompt string, messages []Message, config CompactionConfig) (*CompactionResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to compact")
	}

	originalCount := len(messages)
	sanitized := sanitizeToolHistory(messages)
	inputLimit := config.InputLimit
	if inputLimit <= 0 {
		inputLimit = InputLimitForModel(model)
		config.InputLimit = inputLimit
	}
	prepared := prepareCompactionContext(sanitized, config, "")
	fallbackToHard := func(cause error, softUsage Usage) (*CompactionResult, error) {
		if cause != nil && (errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)) {
			return nil, cause
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		result, hardErr := Compact(ctx, provider, model, systemPrompt, messages, config)
		if hardErr != nil {
			if cause != nil {
				return nil, fmt.Errorf("soft compaction failed: %v; hard fallback failed: %w", cause, hardErr)
			}
			return nil, hardErr
		}
		if !softUsage.IsZero() {
			result.Usage.Add(softUsage)
		}
		return result, nil
	}

	var reqMessages []Message
	if systemPrompt != "" {
		reqMessages = append(reqMessages, SystemText(systemPrompt))
	}
	reqMessages = append(reqMessages, prepareMessagesForSummaryHelper(prepared.SummaryMessages)...)

	if inputLimit > 0 {
		maxInputTokens := inputLimit * 3 / 4
		reqMessages = trimMessagesToFit(reqMessages, maxInputTokens)
	}

	if len(reqMessages) > 0 {
		lastRole := reqMessages[len(reqMessages)-1].Role
		if lastRole == RoleUser || lastRole == RoleTool {
			reqMessages = append(reqMessages, AssistantText("I'll now write the continuation brief."))
		}
	}
	reqMessages = append(reqMessages, UserText(contextContinuationBriefPrompt))

	budget := config.SummaryTokenBudget
	if budget <= 0 {
		budget = defaultSummaryTokenBudget
	}
	budget = ClampOutputTokens(budget, model)

	stream, err := isolatedConversationProvider(provider).Stream(ctx, Request{
		Model:           model,
		Messages:        reqMessages,
		MaxOutputTokens: budget,
	})
	if err != nil {
		return fallbackToHard(fmt.Errorf("soft compaction stream failed: %w", err), Usage{})
	}
	defer stream.Close()

	var brief strings.Builder
	var reasoningSummary strings.Builder
	var usage Usage
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fallbackToHard(fmt.Errorf("soft compaction recv failed: %w", err), usage)
		}
		switch event.Type {
		case EventTextDelta:
			brief.WriteString(event.Text)
		case EventReasoningDelta:
			if isDisplayableReasoningSummaryEvent(event) {
				if event.Text != "" {
					reasoningSummary.WriteString(event.Text)
				}
				if len(event.ReasoningSummaryParts) > 0 {
					reasoningSummary.Reset()
					reasoningSummary.WriteString(strings.Join(event.ReasoningSummaryParts, "\n\n"))
				}
			}
		case EventUsage:
			if event.Use != nil {
				usage.Add(*event.Use)
			}
		case EventAttemptDiscard:
			brief.Reset()
			reasoningSummary.Reset()
			usage = Usage{}
		}
	}

	briefText := continuationBriefFromTextAndReasoning(brief.String(), reasoningSummary.String())
	if briefText == "" {
		return fallbackToHard(fmt.Errorf("soft compaction produced empty brief"), usage)
	}

	result := compactionResultFromBriefPrepared(systemPrompt, briefText, prepared, originalCount, config)
	result.Model = strings.TrimSpace(model)
	result.Usage = usage
	return result, nil
}

func continuationBriefFromTextAndReasoning(text, reasoningSummary string) string {
	if text = strings.TrimSpace(text); text != "" {
		return text
	}
	return strings.TrimSpace(reasoningSummary)
}

func continuationBriefFromAssistantMessage(msg Message) string {
	return continuationBriefFromTextAndReasoning(MessageText(msg), displayableReasoningSummaryText(msg))
}

func displayableReasoningSummaryText(msg Message) string {
	var parts []string
	for _, part := range msg.Parts {
		if len(part.ReasoningSummaryParts) > 0 {
			parts = append(parts, strings.Join(part.ReasoningSummaryParts, "\n\n"))
			continue
		}
		if strings.TrimSpace(part.ReasoningContent) != "" && NormalizeReasoningKind(part.ReasoningKind) == ReasoningKindSummary {
			parts = append(parts, part.ReasoningContent)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func isDisplayableReasoningSummaryEvent(event Event) bool {
	if len(event.ReasoningSummaryParts) > 0 {
		return true
	}
	return event.Text != "" && NormalizeReasoningKind(event.ReasoningKind) == ReasoningKindSummary
}

func staticInfoMaxChars(inputLimit int) int {
	if inputLimit <= 0 {
		return maxPreviousTurnsChars
	}
	// 5% of the provider input window, converted to chars with the
	// approxBytesPerToken heuristic. Use integer arithmetic so absurdly large
	// provider limits cannot overflow before the hard cap is applied.
	windowTokens := inputLimit / 20
	if windowTokens <= 0 {
		return 0
	}
	hardCapTokens := (maxPreviousTurnsChars + approxBytesPerToken - 1) / approxBytesPerToken
	if windowTokens >= hardCapTokens {
		return maxPreviousTurnsChars
	}
	return windowTokens * approxBytesPerToken
}

func buildCompactionStaticInfo(messages []Message, inputLimit int, skipBrief string) string {
	budget := staticInfoMaxChars(inputLimit)
	if budget <= 0 {
		return ""
	}
	messages = filterStaticInfoMessages(messages, skipBrief)
	return buildPreviousTurnsBlock(messages, budget)
}

func buildPreviousTurnsBlock(messages []Message, budget int) string {
	if budget <= 0 {
		return ""
	}
	budget = min(budget, maxPreviousTurnsChars)
	minimal := previousTurnsOpen + strings.TrimPrefix(previousTurnsClose, "\n")
	if budget <= runeLen(minimal) {
		return truncateVisible(minimal, budget)
	}

	bodyBudget := budget - runeLen(previousTurnsOpen) - runeLen(previousTurnsClose)
	if bodyBudget <= 0 {
		return truncateVisible(minimal, budget)
	}

	body := renderRecentPreviousTurns(previousTurnEntries(messages), bodyBudget)
	if strings.TrimSpace(body) == "" {
		return minimal
	}
	body = strings.TrimRight(truncateVisible(body, bodyBudget), "\n")
	return previousTurnsOpen + body + previousTurnsClose
}

type previousTurnEntry struct {
	index              int
	role               Role
	label              string
	text               string
	toolCalls          []string
	toolResults        []string
	latestUser         bool
	internalCompaction bool
	hasImportant       bool
	hasState           bool
	hasTools           bool
	hasToolError       bool
}

type renderedPreviousTurn struct {
	index int
	text  string
}

func previousTurnEntries(messages []Message) []previousTurnEntry {
	latestUserIndex := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser && !isInternalCompactionSummaryMessage(messages[i]) && strings.TrimSpace(messageStaticText(messages[i])) != "" {
			latestUserIndex = i
			break
		}
	}

	entries := make([]previousTurnEntry, 0, len(messages))
	for i, msg := range messages {
		label := roleLabel(msg.Role)
		internalCompaction := isInternalCompactionSummaryMessage(msg)
		if internalCompaction {
			label = "prior compaction summary"
		}
		entry := previousTurnEntry{
			index:              i,
			role:               msg.Role,
			label:              label,
			text:               messageStaticText(msg),
			latestUser:         i == latestUserIndex,
			internalCompaction: internalCompaction,
		}
		for _, part := range msg.Parts {
			if part.ToolCall != nil {
				if summary := compactToolCallSummary(part.ToolCall); summary != "" {
					entry.toolCalls = append(entry.toolCalls, summary)
				}
			}
			if part.ToolResult != nil {
				if summary := compactToolResultSummary(part.ToolResult); summary != "" {
					entry.toolResults = append(entry.toolResults, summary)
				}
				if part.ToolResult.IsError {
					entry.hasToolError = true
				}
			}
		}
		entry.hasTools = len(entry.toolCalls) > 0 || len(entry.toolResults) > 0
		priorityText := entry.text + "\n" + strings.Join(entry.toolCalls, "\n") + "\n" + strings.Join(entry.toolResults, "\n")
		entry.hasImportant = len(newestInterestingLines(priorityText, isImportantLine, 1)) > 0 || len(newestInterestingLines(priorityText, isConstraintLine, 1)) > 0
		entry.hasState = len(newestInterestingLines(priorityText, isAssistantStateLine, 1)) > 0
		if strings.TrimSpace(entry.text) == "" && !entry.hasTools {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

func renderRecentPreviousTurns(entries []previousTurnEntry, bodyBudget int) string {
	if bodyBudget <= 0 || len(entries) == 0 {
		return ""
	}

	// Select turns in reverse chronological order so the newest context wins when
	// the budget is tight. The latest real user turn is pinned first because it is
	// the most important instruction to preserve, even if an assistant/tool turn
	// follows it. Selected entries are rendered chronologically below so the block
	// still reads like a transcript.
	selected := make([]renderedPreviousTurn, 0, len(entries))
	used := 0
	appendEntry := func(entry previousTurnEntry) {
		separatorCost := 0
		if len(selected) > 0 {
			separatorCost = 2 // blank line between selected turns
		}
		remaining := bodyBudget - used - separatorCost
		if remaining <= 0 {
			return
		}

		cap := min(previousTurnEntryCap(entry, bodyBudget), remaining)
		if cap <= 0 {
			return
		}
		rendered := strings.TrimRight(renderPreviousTurnEntry(entry, cap), "\n")
		if rendered == "" {
			return
		}
		if runeLen(rendered) > remaining {
			rendered = truncateVisible(rendered, remaining)
		}
		// When the budget is tiny, prefer a truncated latest user turn over an
		// empty block. Otherwise skip fragments that are too small to be useful.
		if runeLen(rendered) < 40 && len(selected) > 0 && !entry.latestUser {
			return
		}
		selected = append(selected, renderedPreviousTurn{index: entry.index, text: rendered})
		used += separatorCost + runeLen(rendered)
	}

	pinnedLatestUser := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].latestUser {
			appendEntry(entries[i])
			pinnedLatestUser = i
			break
		}
	}

	for i := len(entries) - 1; i >= 0; i-- {
		if i == pinnedLatestUser {
			continue
		}
		if bodyBudget-used <= 0 {
			break
		}
		appendEntry(entries[i])
	}

	sort.SliceStable(selected, func(i, j int) bool { return selected[i].index < selected[j].index })
	parts := make([]string, 0, len(selected))
	for _, item := range selected {
		parts = append(parts, item.text)
	}
	return strings.Join(parts, "\n\n")
}

func previousTurnEntryCap(entry previousTurnEntry, bodyBudget int) int {
	if bodyBudget <= 0 {
		return 0
	}
	if entry.internalCompaction {
		return min(bodyBudget, min(4000, max(180, bodyBudget*24/100)))
	}
	if entry.latestUser {
		return min(bodyBudget, min(6000, max(200, bodyBudget*40/100)))
	}
	if entry.role == RoleUser && entry.hasImportant {
		return min(bodyBudget, min(3500, max(180, bodyBudget*24/100)))
	}
	if entry.hasToolError {
		return min(bodyBudget, min(3000, max(180, bodyBudget*22/100)))
	}
	if entry.hasTools {
		return min(bodyBudget, min(2500, max(160, bodyBudget*18/100)))
	}
	if entry.role == RoleAssistant && (entry.hasImportant || entry.hasState) {
		return min(bodyBudget, min(2200, max(150, bodyBudget*16/100)))
	}
	if entry.role == RoleUser {
		return min(bodyBudget, min(2200, max(140, bodyBudget*14/100)))
	}
	if entry.role == RoleAssistant {
		return min(bodyBudget, min(900, max(100, bodyBudget*8/100)))
	}
	return min(bodyBudget, min(1200, max(100, bodyBudget*10/100)))
}

func renderPreviousTurnEntry(entry previousTurnEntry, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}

	var sections []string
	if strings.TrimSpace(entry.text) != "" && entry.role != RoleTool {
		textCap := previousTurnTextCap(entry, maxChars)
		text := truncateVisible(normalizeSnippetText(entry.text), textCap)
		sections = append(sections, formatPreviousTurnBlock(entry.label, text))
	}
	if len(entry.toolCalls) > 0 {
		sections = append(sections, formatPreviousTurnList("tool calls", entry.toolCalls, 6))
	}
	if len(entry.toolResults) > 0 {
		sections = append(sections, formatPreviousTurnList("tool results", entry.toolResults, 6))
	}
	if strings.TrimSpace(entry.text) != "" && entry.role == RoleTool && len(entry.toolResults) == 0 {
		text := truncateVisible(normalizeSnippetText(entry.text), previousTurnTextCap(entry, maxChars))
		sections = append(sections, formatPreviousTurnBlock("tool", text))
	}

	rendered := strings.TrimSpace(strings.Join(sections, "\n"))
	if rendered == "" {
		return ""
	}
	return truncateVisible(rendered, maxChars)
}

func previousTurnTextCap(entry previousTurnEntry, maxChars int) int {
	if maxChars <= 0 {
		return 0
	}
	reserved := 0
	if len(entry.toolCalls) > 0 {
		reserved += min(500, maxChars/4)
	}
	if len(entry.toolResults) > 0 {
		reserved += min(700, maxChars/3)
	}
	cap := maxChars - reserved
	if cap < maxChars/2 {
		cap = maxChars / 2
	}
	if entry.latestUser {
		return min(maxChars, max(200, cap))
	}
	if entry.role == RoleAssistant && !entry.hasImportant && !entry.hasState {
		return min(maxChars, min(cap, 600))
	}
	return min(maxChars, max(120, cap))
}

func roleLabel(role Role) string {
	switch role {
	case RoleUser:
		return "user"
	case RoleAssistant:
		return "assistant"
	case RoleTool:
		return "tool"
	default:
		return string(role)
	}
}

func formatPreviousTurnBlock(label, text string) string {
	text = normalizeSnippetText(text)
	if text == "" {
		return ""
	}
	if !strings.Contains(text, "\n") && runeLen(text) <= 180 {
		return label + ": " + text
	}
	return label + ":\n" + indentPreviousTurnText(text)
}

func formatPreviousTurnList(label string, items []string, limit int) string {
	if len(items) == 0 || limit <= 0 {
		return ""
	}
	var lines []string
	for i, item := range items {
		if i >= limit {
			lines = append(lines, fmt.Sprintf("  - ... %d more", len(items)-limit))
			break
		}
		item = normalizeSnippetText(item)
		if item == "" {
			continue
		}
		lines = append(lines, "  - "+truncateVisible(singleLine(item), 360))
	}
	if len(lines) == 0 {
		return ""
	}
	return label + ":\n" + strings.Join(lines, "\n")
}

func indentPreviousTurnText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}

func singleLine(text string) string {
	text = normalizeSnippetText(text)
	text = strings.ReplaceAll(text, "\n", " ")
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}
	return strings.TrimSpace(text)
}

func compactToolCallSummary(call *ToolCall) string {
	if call == nil {
		return ""
	}
	name := strings.TrimSpace(call.Name)
	if name == "" {
		name = "tool"
	}
	var details []string
	if info := singleLine(call.ToolInfo); info != "" {
		details = append(details, truncateVisible(info, 160))
	}
	if args := compactToolArgs(call.Arguments); args != "" {
		details = append(details, args)
	}
	if len(details) == 0 {
		return name
	}
	return name + " " + strings.Join(details, " ")
}

func compactToolArgs(raw json.RawMessage) string {
	rawText := strings.TrimSpace(string(raw))
	if rawText == "" || rawText == "{}" || rawText == "null" {
		return ""
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil && len(obj) > 0 {
		parts := prioritizedToolArgParts(obj)
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}

	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err == nil {
		rawText = compacted.String()
	}
	return "args=" + truncateVisible(singleLine(rawText), 240)
}

func prioritizedToolArgParts(obj map[string]any) []string {
	priorityKeys := []string{"path", "file", "filename", "directory", "cwd", "working_dir", "pattern", "query", "url", "command", "cmd", "description"}
	seen := make(map[string]bool, len(priorityKeys))
	parts := make([]string, 0, 4)
	appendKey := func(key string) {
		value, ok := obj[key]
		if !ok || seen[key] || len(parts) >= 4 {
			return
		}
		if brief := briefToolArgValue(key, value); brief != "" {
			parts = append(parts, key+"="+brief)
			seen[key] = true
		}
	}
	for _, key := range priorityKeys {
		appendKey(key)
	}
	if len(parts) >= 2 {
		return parts
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if isVerboseToolArgKey(key) {
			continue
		}
		appendKey(key)
	}
	return parts
}

func briefToolArgValue(key string, value any) string {
	switch v := value.(type) {
	case string:
		v = singleLine(v)
		if v == "" {
			return ""
		}
		if isVerboseToolArgKey(key) && runeLen(v) > 80 {
			return fmt.Sprintf("%d chars", runeLen(v))
		}
		return truncateVisible(v, 160)
	case float64, bool:
		return fmt.Sprintf("%v", v)
	case []any:
		if len(v) == 0 {
			return ""
		}
		return fmt.Sprintf("%d items", len(v))
	case map[string]any:
		if len(v) == 0 {
			return ""
		}
		buf, _ := json.Marshal(v)
		return truncateVisible(singleLine(string(buf)), 160)
	default:
		buf, _ := json.Marshal(v)
		return truncateVisible(singleLine(string(buf)), 160)
	}
}

func isVerboseToolArgKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "content" || key == "text" || key == "old_text" || key == "new_text" || strings.Contains(key, "body")
}

func compactToolResultSummary(result *ToolResult) string {
	if result == nil {
		return ""
	}
	name := strings.TrimSpace(result.Name)
	if name == "" {
		name = "tool"
	}
	state := "completed"
	if result.IsError {
		state = "error"
	}
	text := name + " " + state

	var details []string
	if len(result.Diffs) > 0 {
		files := make([]string, 0, len(result.Diffs))
		seen := map[string]bool{}
		for _, diff := range result.Diffs {
			file := strings.TrimSpace(diff.File)
			if file != "" && !seen[file] {
				seen[file] = true
				files = append(files, file)
			}
		}
		if len(files) > 0 {
			details = append(details, "diffs: "+truncateVisible(strings.Join(files, ", "), 240))
		}
	}
	if len(result.Images) > 0 {
		details = append(details, fmt.Sprintf("%d image(s)", len(result.Images)))
	}

	content := toolResultTextContent(result)
	contentLen := runeLen(strings.TrimSpace(content))
	if contentLen > 0 && shouldSuppressToolResultContent(name) && !result.IsError {
		details = append(details, fmt.Sprintf("bulk output omitted; %d chars", contentLen))
		if len(details) > 0 {
			text += ": " + strings.Join(details, "; ")
		}
		return truncateVisible(text, 1000)
	}
	if important := strings.Join(newestInterestingLines(content, isImportantLine, 4), "; "); important != "" {
		details = append(details, important)
	} else if first := firstNonEmptyLine(content); first != "" {
		details = append(details, truncateVisible(singleLine(first), 220))
	}
	if contentLen > 0 {
		firstLen := runeLen(firstNonEmptyLine(content))
		if contentLen > firstLen+200 || strings.Count(content, "\n") > 20 {
			details = append(details, fmt.Sprintf("bulk output omitted; %d chars", contentLen))
		}
	}
	if len(details) > 0 {
		text += ": " + strings.Join(details, "; ")
	}
	return truncateVisible(text, 1000)
}

func shouldSuppressToolResultContent(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read_file", "read_url", "view_image":
		return true
	default:
		return false
	}
}

func filterStaticInfoMessages(messages []Message, skipBrief string) []Message {
	brief := strings.TrimSpace(skipBrief)
	skipBriefIndex := -1
	if brief != "" {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == RoleAssistant && strings.TrimSpace(messageStaticText(messages[i])) == brief {
				skipBriefIndex = i
				break
			}
		}
	}
	filtered := make([]Message, 0, len(messages))
	for i, msg := range messages {
		if msg.Role == RoleSystem || msg.Role == RoleDeveloper || msg.Role == RoleEvent {
			continue
		}
		text := strings.TrimSpace(messageStaticText(msg))
		if text == "" && !messageHasToolMetadata(msg) {
			continue
		}
		if isCompactionControlPromptText(MessageText(msg)) {
			continue
		}
		if i == skipBriefIndex {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func messageHasToolMetadata(msg Message) bool {
	for _, part := range msg.Parts {
		if part.ToolCall != nil || part.ToolResult != nil {
			return true
		}
	}
	return false
}

func messageStaticText(msg Message) string {
	var parts []string
	text := strings.TrimSpace(MessageText(msg))
	if summary, ok := compactionSummaryTextForStaticInfo(text); ok {
		text = summary
	}
	if text != "" {
		parts = append(parts, text)
	}
	attachment := MessageAttachmentSummary(msg)
	if attachment != "" {
		parts = append(parts, attachment)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func isInternalCompactionSummaryMessage(msg Message) bool {
	return isInternalCompactionSummaryText(MessageText(msg))
}

func isInternalCompactionSummaryText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "[Context Compaction]")
}

// IsInternalCompactionSummaryText reports whether text is an internal context
// compaction summary message. UI layers use this to hide the bulky internal
// prompt in normal chat while still exposing it in inspectors/debug views.
func IsInternalCompactionSummaryText(text string) bool {
	return isInternalCompactionSummaryText(text)
}

// CompactionSummaryDisplayText extracts the human-authored summary/action block
// from an internal compaction message. If the message uses the older untagged
// format, deterministic PREVIOUS_TURNS transcript data is stripped so UI
// placeholders describe the concise summary instead of bulky internal context.
func CompactionSummaryDisplayText(text string) string {
	if summary, ok := compactionSummaryTextForStaticInfo(text); ok {
		return summary
	}
	return normalizeSnippetText(text)
}

func compactionSummaryTextForStaticInfo(text string) (string, bool) {
	if !isInternalCompactionSummaryText(text) {
		return "", false
	}

	text = stripCompactionSummaryPrefix(text)
	if summary, ok := extractTaggedBlock(text, "<SUMMARY_AND_NEXT_ACTIONS>", "</SUMMARY_AND_NEXT_ACTIONS>"); ok {
		return normalizeSnippetText(summary), true
	}

	// Older compacted summaries may not have the final summary tag. Do not carry
	// their deterministic previous-turn transcript forward verbatim; that creates
	// nested <PREVIOUS_TURNS> blocks full of stale tool output.
	text = stripTaggedBlock(text, "<PREVIOUS_TURNS>", "</PREVIOUS_TURNS>")
	return normalizeSnippetText(text), true
}

func stripCompactionSummaryPrefix(text string) string {
	text = strings.TrimSpace(text)
	trimmedPrefix := strings.TrimSpace(summaryPrefix)
	if strings.HasPrefix(text, trimmedPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(text, trimmedPrefix))
	}
	if strings.HasPrefix(text, "[Context Compaction]") {
		if idx := strings.Index(text, "\n\n"); idx >= 0 {
			return strings.TrimSpace(text[idx+2:])
		}
	}
	return text
}

func extractTaggedBlock(text, openTag, closeTag string) (string, bool) {
	start := strings.Index(text, openTag)
	if start < 0 {
		return "", false
	}
	contentStart := start + len(openTag)
	end := strings.Index(text[contentStart:], closeTag)
	if end < 0 {
		return strings.TrimSpace(text[contentStart:]), true
	}
	return strings.TrimSpace(text[contentStart : contentStart+end]), true
}

func stripTaggedBlock(text, openTag, closeTag string) string {
	for {
		start := strings.Index(text, openTag)
		if start < 0 {
			return strings.TrimSpace(text)
		}
		contentStart := start + len(openTag)
		end := strings.Index(text[contentStart:], closeTag)
		if end < 0 {
			return strings.TrimSpace(text[:start])
		}
		end += contentStart + len(closeTag)
		text = text[:start] + text[end:]
	}
}

func normalizeSnippetText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func newestInterestingLines(text string, pred func(string) bool, limit int) []string {
	text = normalizeSnippetText(text)
	if text == "" || limit <= 0 {
		return nil
	}
	lines := splitSnippetCandidates(text)
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if pred(line) {
			out = append(out, line)
		}
	}
	return out
}

func splitSnippetCandidates(text string) []string {
	lines := strings.Split(text, "\n")
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if runeLen(line) > 500 {
			out = append(out, truncateVisible(line, 500))
		} else {
			out = append(out, line)
		}
	}
	if len(out) <= 1 {
		// For single-line user prompts, also inspect simple sentence boundaries.
		for _, piece := range strings.Split(text, ". ") {
			piece = strings.TrimSpace(piece)
			if piece != "" {
				out = append(out, piece)
			}
		}
	}
	return out
}

func isConstraintLine(line string) bool {
	lower := strings.ToLower(line)
	markers := []string{"must", "do not", "don't", "never", "always", "prefer", "preference", "constraint", "require", "requirement", "please", "avoid", "ask", "question", "?"}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isImportantLine(line string) bool {
	lower := strings.ToLower(line)
	markers := []string{
		"error", "failed", "failure", "panic", "traceback", "exception", "context_length_exceeded",
		"go test", "go build", "npm test", "pytest", "cargo test", "failing test", "failed test", "exit_code", "benchmark",
		".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rb", ".rs", ".java", ".md", ".json", ".yaml", ".yml", ".toml", ".sql", ".sh",
		"internal/", "cmd/", "pkg/", "src/", "func ", "function", "config", "setting", "flag", "env", "line ",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.Contains(line, "/") || strings.Contains(line, "\\") || strings.Contains(line, "Test")
}

func isAssistantStateLine(line string) bool {
	lower := strings.ToLower(line)
	markers := []string{"next", "todo", "pending", "blocked", "blocker", "decision", "decided", "done", "completed", "current state", "found", "need to", "will ", "plan", "fix"}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func appendRawLimited(b *strings.Builder, text string, maxChars int) {
	remaining := maxChars - runeLen(b.String())
	if remaining <= 0 {
		return
	}
	b.WriteString(truncateVisible(text, remaining))
}

func truncateVisible(text string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	marker := "...[truncated]"
	markerRunes := []rune(marker)
	if maxChars <= len(markerRunes)+2 {
		return string(runes[:maxChars])
	}
	available := maxChars - len(markerRunes)
	head := available / 2
	tail := available - head
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}

func runeLen(s string) int { return len([]rune(s)) }

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

// Compact generates a structured continuation brief for the older conversation
// prefix and returns a compacted message list: [system] + [tagged summary as
// user] + [ack if needed] + [bounded recent raw suffix].
//
// The tagged summary uses the same shape as soft compaction: deterministic
// <PREVIOUS_TURNS> excerpts from the summarized prefix plus the model-written
// <SUMMARY_AND_NEXT_ACTIONS> brief. The raw suffix is not sent to the helper,
// avoiding extractive-summary overlap while preserving exact recent
// user/assistant/tool structure for continuation.
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
	inputLimit := config.InputLimit
	if inputLimit <= 0 {
		inputLimit = InputLimitForModel(model)
		config.InputLimit = inputLimit
	}
	prepared := prepareCompactionContext(messages, config, "")

	// Build request: [system] + summary prefix messages + compaction instruction.
	var reqMessages []Message
	if systemPrompt != "" {
		reqMessages = append(reqMessages, SystemText(systemPrompt))
	}
	reqMessages = append(reqMessages, prepareMessagesForSummaryHelper(prepared.SummaryMessages)...)

	// If messages exceed the input limit, trim from the front (after system)
	// to fit. This handles reactive compaction where we're already at or past
	// the context window. Keep ~75% of the input limit for conversation
	// messages, reserving the rest for the output budget and framing.
	// Use provider-effective limit if set, else fall back to canonical.
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

	// Collect the structured continuation brief.
	var brief strings.Builder
	var reasoningSummary strings.Builder
	var usage Usage
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("compaction recv failed: %w", err)
		}
		switch event.Type {
		case EventTextDelta:
			brief.WriteString(event.Text)
		case EventReasoningDelta:
			if isDisplayableReasoningSummaryEvent(event) {
				if event.Text != "" {
					reasoningSummary.WriteString(event.Text)
				}
				if len(event.ReasoningSummaryParts) > 0 {
					reasoningSummary.Reset()
					reasoningSummary.WriteString(strings.Join(event.ReasoningSummaryParts, "\n\n"))
				}
			}
		case EventUsage:
			if event.Use != nil {
				usage.Add(*event.Use)
			}
		case EventAttemptDiscard:
			brief.Reset()
			reasoningSummary.Reset()
			usage = Usage{}
		}
	}

	briefText := continuationBriefFromTextAndReasoning(brief.String(), reasoningSummary.String())
	if briefText == "" {
		return nil, fmt.Errorf("compaction produced empty brief")
	}

	// Reconstruct history with the same tagged summary shape used by soft
	// compaction: deterministic previous-turn excerpts, the model-written
	// continuation brief, then a bounded raw suffix.
	result := compactionResultFromBriefPrepared(systemPrompt, briefText, prepared, originalCount, config)
	result.Model = strings.TrimSpace(model)
	result.Usage = usage
	return result, nil
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
// [SystemText(newSystemPrompt)] + [handover doc (user, CacheAnchor)] + [assistant ack].
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
	prefixTokens := EstimateMessageTokens(prefix)
	convTokens := EstimateMessageTokens(conv)
	for len(conv) > 1 && prefixTokens+convTokens > maxTokens {
		convTokens -= estimateSingleMessageTokens(conv[0])
		conv = conv[1:]
	}

	// If a single message exceeds budget, truncate its text/tool content.
	if len(conv) == 1 {
		if prefixTokens+convTokens > maxTokens {
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
		if part.ToolResult != nil {
			tr := truncateToolResult(*part.ToolResult, maxChars)
			result.Parts[i].ToolResult = &tr
		}
	}
	return result
}

// reconstructHistory builds the compacted message list:
// [SystemText(systemPrompt)] + [summary(user, CacheAnchor)] + [ack if needed] + [recent raw context]
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

	// Add an assistant acknowledgement unless the retained raw suffix already
	// starts with an assistant message. This avoids consecutive assistant turns in
	// the common split-suffix case while still preventing summary-user + recent-user
	// from being interpreted as a brand-new user request pair.
	if len(recentMsgs) == 0 || recentMsgs[0].Role != RoleAssistant {
		messages = append(messages, AssistantText("I've reviewed the context summary. I'll continue from where we left off."))
	}

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
