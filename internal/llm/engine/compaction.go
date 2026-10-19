package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

const (
	// DefaultThresholdRatio is the legacy default compaction trigger fraction.
	DefaultThresholdRatio     = 0.90
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

// CompactionAckText is the internal assistant acknowledgement inserted after a
// compaction summary. Consumers detect the synthetic turn by exact text, so it
// must have a single definition: session tail marking and the web transcript
// both key off this literal.
const CompactionAckText = "I've reviewed the context summary. I'll continue from where we left off."

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
		ThresholdRatio:       DefaultThresholdRatio,
		SoftThresholdRatio:   defaultSoftThresholdRatio,
		HardThresholdRatio:   defaultHardThresholdRatio,
		MaxToolResultChars:   defaultMaxToolResultChars,
		SummaryTokenBudget:   defaultSummaryTokenBudget,
		RecentRawTokenBudget: 0, // auto-size from provider input window, capped at 8k
		RecentRawTurns:       defaultRecentRawTurns,
	}
}

// EffectiveCompactionThresholdRatios resolves the soft and hard trigger ratios.
func EffectiveCompactionThresholdRatios(config *CompactionConfig) (soft, hard float64) {
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
	Summary           string
	NewMessages       []core.Message
	EphemeralMessages []core.Message
	OriginalCount     int
	CompactedCount    int
	Model             string     // Model used by the helper LLM call.
	Usage             core.Usage // Token usage/cost of the helper LLM call that produced the summary.
}

// ActiveMessages returns the durable replacement history plus request-only
// restoration context. Developer context is placed before the latest user turn
// so providers without a native developer role can fold it into that turn. The
// ordinary no-ephemeral path returns NewMessages directly, preserving allocation
// and identity behavior for existing agents.
func (r *CompactionResult) ActiveMessages() []core.Message {
	if r == nil {
		return nil
	}
	if len(r.EphemeralMessages) == 0 {
		return r.NewMessages
	}
	active := append([]core.Message(nil), r.NewMessages...)
	for _, ephemeral := range r.EphemeralMessages {
		if ephemeral.Role != core.RoleDeveloper {
			active = append(active, ephemeral)
			continue
		}
		insertAt := len(active)
		for i := len(active) - 1; i >= 0; i-- {
			if active[i].Role == core.RoleUser {
				insertAt = i
				break
			}
		}
		active = append(active, core.Message{})
		copy(active[insertAt+1:], active[insertAt:])
		active[insertAt] = ephemeral
	}
	return active
}

// EstimateTokens returns an approximate token count for a string using a
// simple 4-bytes-per-token heuristic (same as codex).
func EstimateTokens(text string) int {
	return (len(text) + approxBytesPerToken - 1) / approxBytesPerToken
}

// EstimateMessageTokens returns an approximate token count for a slice of
// messages by summing all text content across parts.
func EstimateMessageTokens(msgs []core.Message) int {
	if len(msgs) == 0 {
		return 0
	}
	total := 0
	for _, msg := range msgs {
		total += estimateSingleMessageTokens(msg)
	}
	return total
}

func estimateSingleMessageTokens(msg core.Message) int {
	if msg.Role == core.RoleEvent || len(msg.Parts) == 0 {
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

// PreparedCompactionContext is the split used by both hard and soft
// compaction: summarize the old prefix, then replay a bounded recent suffix as
// true structured messages after the summary. Keeping the split in one place
// prevents hard/soft modes from drifting and avoids duplicating recent context
// inside the extractive <PREVIOUS_TURNS> block.
type PreparedCompactionContext struct {
	SummaryMessages   []core.Message
	RecentMessages    []core.Message
	ConversationStart []core.Message
	PlatformContext   []core.Message
}

// PrepareCompactionContext splits messages into summary and raw-suffix sets.
func (e CompactionEnv) PrepareCompactionContext(messages []core.Message, config CompactionConfig, skipBrief string) PreparedCompactionContext {
	var conversationStart []core.Message
	if message, ok := e.conversationStartFrom(messages); ok {
		conversationStart = []core.Message{message}
	}
	var platformContext []core.Message
	if message, ok := e.platformContextFrom(messages); ok {
		platformContext = []core.Message{message}
	}
	prepared := PreparedCompactionContext{ConversationStart: conversationStart, PlatformContext: platformContext}
	source := filterCompactionControlMessages(messages, skipBrief)
	if len(source) == 0 {
		return prepared
	}

	budget := effectiveRecentRawTokenBudget(config)
	turns := effectiveRecentRawTurns(config)
	if budget <= 0 || turns <= 0 || len(source) <= 1 {
		prepared.SummaryMessages = source
		return prepared
	}

	start := selectRecentRawSuffixStart(source, budget, turns)
	if start <= 0 || start >= len(source) {
		prepared.SummaryMessages = source
		return prepared
	}

	summaryMessages := source[:start]
	if !hasCompactionSummaryInput(summaryMessages) {
		// If the split would leave nothing meaningful to summarize, keep the old
		// summary-only behavior. This avoids compacting tiny one-turn histories into
		// summary+duplicate raw messages.
		prepared.SummaryMessages = source
		return prepared
	}

	recent := sanitizeRecentRawSuffix(source[start:])
	if len(recent) == 0 {
		prepared.SummaryMessages = source
		return prepared
	}
	prepared.SummaryMessages = summaryMessages
	prepared.RecentMessages = recent
	return prepared
}

func filterCompactionControlMessages(messages []core.Message, skipBrief string) []core.Message {
	brief := strings.TrimSpace(skipBrief)
	filtered := core.FilterConversationMessages(messages)
	skipBriefIndex := -1
	if brief != "" {
		for i := len(filtered) - 1; i >= 0; i-- {
			msg := filtered[i]
			if msg.Role == core.RoleAssistant && strings.TrimSpace(ContinuationBriefFromAssistantMessage(msg)) == brief {
				skipBriefIndex = i
				break
			}
		}
	}
	out := make([]core.Message, 0, len(filtered))
	for i, msg := range filtered {
		if msg.Role == core.RoleSystem || msg.Role == core.RoleDeveloper || msg.Role == core.RoleEvent {
			continue
		}
		if isCompactionControlPromptText(core.MessageText(msg)) {
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
	return text == strings.TrimSpace(ContextContinuationBriefPrompt) || text == strings.TrimSpace(ContextContinuationPrompt)
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

func selectRecentRawSuffixStart(messages []core.Message, budget, maxTurns int) int {
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

func realUserTurnStarts(messages []core.Message) []int {
	starts := make([]int, 0, len(messages))
	for i, msg := range messages {
		if msg.Role == core.RoleUser && !isInternalCompactionSummaryMessage(msg) && strings.TrimSpace(core.MessageText(msg)) != "" {
			starts = append(starts, i)
		}
	}
	return starts
}

func isValidRecentRawStart(msg core.Message) bool {
	switch msg.Role {
	case core.RoleUser:
		return !isInternalCompactionSummaryMessage(msg)
	case core.RoleAssistant:
		return true
	default:
		return false
	}
}

func sanitizeRecentRawSuffix(messages []core.Message) []core.Message {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]core.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == core.RoleSystem || msg.Role == core.RoleDeveloper || msg.Role == core.RoleEvent {
			continue
		}
		parts := core.CloneParts(msg.Parts)
		if len(parts) == 0 {
			continue
		}
		copyMsg := core.Message{Role: msg.Role, Parts: parts}
		// The compacted summary is the cache anchor. Do not carry any stale anchor
		// from a retained raw suffix into the reconstructed context.
		copyMsg.CacheAnchor = false
		cloned = append(cloned, copyMsg)
	}
	return dropEmptyMessages(core.SanitizeToolHistory(cloned))
}

func dropEmptyMessages(messages []core.Message) []core.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]core.Message, 0, len(messages))
	for _, msg := range messages {
		if len(msg.Parts) == 0 {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func hasCompactionSummaryInput(messages []core.Message) bool {
	for _, msg := range messages {
		if msg.Role == core.RoleSystem || msg.Role == core.RoleDeveloper || msg.Role == core.RoleEvent {
			continue
		}
		if strings.TrimSpace(core.MessageText(msg)) != "" || messageHasToolMetadata(msg) {
			return true
		}
	}
	return false
}

func prepareMessagesForSummaryHelper(messages []core.Message) []core.Message {
	messages = core.SanitizeToolHistory(messages)
	out := make([]core.Message, 0, len(messages))
	for _, msg := range messages {
		copyMsg := core.Message{Role: msg.Role, CacheAnchor: msg.CacheAnchor, Parts: core.CloneParts(msg.Parts)}
		for i := range copyMsg.Parts {
			part := &copyMsg.Parts[i]
			if part.Type == core.PartText || part.Type == core.PartFile {
				if summary, ok := compactionSummaryTextForStaticInfo(part.Text); ok {
					part.Text = SummaryPrefix + summary
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

// CompactionPrompt is the hard-compaction summary request.
const CompactionPrompt = `Create a compact continuation brief for the conversation history you are given. This brief will replace older context. Newer messages may be retained separately as exact raw structured context after the brief, so focus on durable state and avoid duplicating recent raw context unless it is needed for continuity.

` + structuredCompactionBriefInstructions

// SummaryPrefix marks the tagged compaction summary turn.
const SummaryPrefix = `[Context Compaction]
Internal context only; not a user command, stop/cancel/wait request. Continue from the latest real user instruction.

`

// ContextContinuationBriefPrompt is the soft-compaction brief request.
const ContextContinuationBriefPrompt = `Context budget is getting tight. Create a compact continuation brief for the conversation history you are given. Newer messages may be retained separately as exact raw structured context after the brief, so focus on durable state and avoid duplicating recent raw context unless it is needed for continuity.

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
func BuildCompactionStaticInfo(messages []core.Message, inputLimit int) string {
	return buildCompactionStaticInfo(messages, inputLimit, "")
}

// CompactionResultFromBrief builds a normal CompactionResult from a
// continuation brief that has already been produced by an LLM. It intentionally
// avoids a second summary-helper LLM call: a deterministic <PREVIOUS_TURNS>
// block from the summarized prefix followed by the LLM's Summary / Next Actions
// becomes the compacted summary, then a bounded recent raw suffix is replayed as
// true structured messages.
func (e CompactionEnv) CompactionResultFromBrief(systemPrompt, brief string, messages []core.Message, config CompactionConfig) *CompactionResult {
	brief = strings.TrimSpace(brief)
	prepared := e.PrepareCompactionContext(messages, config, brief)
	return e.CompactionResultFromBriefPrepared(systemPrompt, brief, prepared, len(messages), config)
}

// CompactionResultFromBriefPrepared builds a result from a prepared split.
func (e CompactionEnv) CompactionResultFromBriefPrepared(systemPrompt, brief string, prepared PreparedCompactionContext, originalCount int, config CompactionConfig) *CompactionResult {
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
	newMessages = e.insertPlatformContext(newMessages, prepared.PlatformContext)
	newMessages = e.insertConversationStart(newMessages, prepared.ConversationStart)
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
func (e CompactionEnv) SoftCompact(ctx context.Context, provider core.Provider, model, systemPrompt string, messages []core.Message, config CompactionConfig) (*CompactionResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to compact")
	}

	originalCount := len(messages)
	sanitized := core.SanitizeToolHistory(messages)
	inputLimit := config.InputLimit
	if inputLimit <= 0 {
		inputLimit = e.inputLimitForModel(model)
		config.InputLimit = inputLimit
	}
	prepared := e.PrepareCompactionContext(sanitized, config, "")
	fallbackToHard := func(cause error, softUsage core.Usage) (*CompactionResult, error) {
		if cause != nil && (errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)) {
			return nil, cause
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		result, hardErr := e.Compact(ctx, provider, model, systemPrompt, messages, config)
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

	reqMessages := buildHelperRequestMessages(
		systemPrompt,
		prepareMessagesForSummaryHelper(prepared.SummaryMessages),
		inputLimit,
		"I'll now write the continuation brief.",
		core.UserText(ContextContinuationBriefPrompt),
	)

	budget := config.SummaryTokenBudget
	if budget <= 0 {
		budget = defaultSummaryTokenBudget
	}
	budget = e.clampOutputTokens(budget, model)

	stream, err := e.isolatedProvider(provider).Stream(ctx, core.Request{
		Model:           model,
		Messages:        reqMessages,
		MaxOutputTokens: budget,
		MaxTurns:        1,
		Ephemeral:       true,
	})
	if err != nil {
		return fallbackToHard(fmt.Errorf("soft compaction stream failed: %w", err), core.Usage{})
	}
	defer stream.Close()

	collected, err := core.CollectTextStream(stream, nil)
	if err != nil {
		return fallbackToHard(fmt.Errorf("soft compaction recv failed: %w", err), collected.Usage)
	}

	briefText := continuationBriefFromTextAndReasoning(collected.Text, collected.ReasoningSummary)
	if briefText == "" {
		return fallbackToHard(fmt.Errorf("soft compaction produced empty brief"), collected.Usage)
	}

	result := e.CompactionResultFromBriefPrepared(systemPrompt, briefText, prepared, originalCount, config)
	result.Model = strings.TrimSpace(model)
	result.Usage = collected.Usage
	return result, nil
}

func continuationBriefFromTextAndReasoning(text, reasoningSummary string) string {
	if text = strings.TrimSpace(text); text != "" {
		return text
	}
	return strings.TrimSpace(reasoningSummary)
}

// ContinuationBriefFromAssistantMessage extracts a brief from an assistant turn.
func ContinuationBriefFromAssistantMessage(msg core.Message) string {
	return continuationBriefFromTextAndReasoning(core.MessageText(msg), displayableReasoningSummaryText(msg))
}

func displayableReasoningSummaryText(msg core.Message) string {
	var parts []string
	for _, part := range msg.Parts {
		if len(part.ReasoningSummaryParts) > 0 {
			parts = append(parts, strings.Join(part.ReasoningSummaryParts, "\n\n"))
			continue
		}
		if strings.TrimSpace(part.ReasoningContent) != "" && core.NormalizeReasoningKind(part.ReasoningKind) == core.ReasoningKindSummary {
			parts = append(parts, part.ReasoningContent)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
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

func buildCompactionStaticInfo(messages []core.Message, inputLimit int, skipBrief string) string {
	budget := staticInfoMaxChars(inputLimit)
	if budget <= 0 {
		return ""
	}
	messages = filterStaticInfoMessages(messages, skipBrief)
	return buildPreviousTurnsBlock(messages, budget)
}

func buildPreviousTurnsBlock(messages []core.Message, budget int) string {
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
	role               core.Role
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

func previousTurnEntries(messages []core.Message) []previousTurnEntry {
	latestUserIndex := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == core.RoleUser && !isInternalCompactionSummaryMessage(messages[i]) && strings.TrimSpace(messageStaticText(messages[i])) != "" {
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
	if entry.role == core.RoleUser && entry.hasImportant {
		return min(bodyBudget, min(3500, max(180, bodyBudget*24/100)))
	}
	if entry.hasToolError {
		return min(bodyBudget, min(3000, max(180, bodyBudget*22/100)))
	}
	if entry.hasTools {
		return min(bodyBudget, min(2500, max(160, bodyBudget*18/100)))
	}
	if entry.role == core.RoleAssistant && (entry.hasImportant || entry.hasState) {
		return min(bodyBudget, min(2200, max(150, bodyBudget*16/100)))
	}
	if entry.role == core.RoleUser {
		return min(bodyBudget, min(2200, max(140, bodyBudget*14/100)))
	}
	if entry.role == core.RoleAssistant {
		return min(bodyBudget, min(900, max(100, bodyBudget*8/100)))
	}
	return min(bodyBudget, min(1200, max(100, bodyBudget*10/100)))
}

func renderPreviousTurnEntry(entry previousTurnEntry, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}

	var sections []string
	if strings.TrimSpace(entry.text) != "" && entry.role != core.RoleTool {
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
	if strings.TrimSpace(entry.text) != "" && entry.role == core.RoleTool && len(entry.toolResults) == 0 {
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
	if entry.role == core.RoleAssistant && !entry.hasImportant && !entry.hasState {
		return min(maxChars, min(cap, 600))
	}
	return min(maxChars, max(120, cap))
}

func roleLabel(role core.Role) string {
	switch role {
	case core.RoleUser:
		return "user"
	case core.RoleAssistant:
		return "assistant"
	case core.RoleTool:
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
