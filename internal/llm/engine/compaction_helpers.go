package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func compactToolCallSummary(call *core.ToolCall) string {
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

func compactToolResultSummary(result *core.ToolResult) string {
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
	media := core.NormalizeMedia(result.Media, result.Images)
	details = append(details, compactMediaSummary(media)...)

	content := core.ToolResultTextContent(result)
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

func filterStaticInfoMessages(messages []core.Message, skipBrief string) []core.Message {
	brief := strings.TrimSpace(skipBrief)
	skipBriefIndex := -1
	if brief != "" {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == core.RoleAssistant && strings.TrimSpace(messageStaticText(messages[i])) == brief {
				skipBriefIndex = i
				break
			}
		}
	}
	filtered := make([]core.Message, 0, len(messages))
	for i, msg := range messages {
		if msg.Role == core.RoleSystem || msg.Role == core.RoleDeveloper || msg.Role == core.RoleEvent {
			continue
		}
		text := strings.TrimSpace(messageStaticText(msg))
		if text == "" && !messageHasToolMetadata(msg) {
			continue
		}
		if isCompactionControlPromptText(core.MessageText(msg)) {
			continue
		}
		if i == skipBriefIndex {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func messageHasToolMetadata(msg core.Message) bool {
	for _, part := range msg.Parts {
		if part.ToolCall != nil || part.ToolResult != nil {
			return true
		}
	}
	return false
}

func messageStaticText(msg core.Message) string {
	var parts []string
	text := strings.TrimSpace(core.MessageText(msg))
	if summary, ok := compactionSummaryTextForStaticInfo(text); ok {
		text = summary
	}
	if text != "" {
		parts = append(parts, text)
	}
	attachment := core.MessageAttachmentSummary(msg)
	if attachment != "" {
		parts = append(parts, attachment)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func isInternalCompactionSummaryMessage(msg core.Message) bool {
	return isInternalCompactionSummaryText(core.MessageText(msg))
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
	trimmedPrefix := strings.TrimSpace(SummaryPrefix)
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

// buildHelperRequestMessages constructs a bounded one-shot helper request. The
// trailing framing should end in a user turn; bridge is inserted when history
// otherwise ends in user/tool so provider role alternation remains valid.
func buildHelperRequestMessages(systemPrompt string, history []core.Message, inputLimit int, bridge string, trailing ...core.Message) []core.Message {
	capacity := len(history) + len(trailing) + 2
	reqMessages := make([]core.Message, 0, capacity)
	if strings.TrimSpace(systemPrompt) != "" {
		reqMessages = append(reqMessages, core.SystemText(systemPrompt))
	}
	reqMessages = append(reqMessages, history...)
	if inputLimit > 0 {
		reqMessages = trimMessagesToFit(reqMessages, inputLimit*3/4)
	}
	if len(reqMessages) > 0 {
		lastRole := reqMessages[len(reqMessages)-1].Role
		if (lastRole == core.RoleUser || lastRole == core.RoleTool) && strings.TrimSpace(bridge) != "" {
			reqMessages = append(reqMessages, core.AssistantText(bridge))
		}
	}
	return append(reqMessages, trailing...)
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
func (e CompactionEnv) Compact(ctx context.Context, provider core.Provider, model, systemPrompt string, messages []core.Message, config CompactionConfig) (*CompactionResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to compact")
	}

	originalCount := len(messages)
	messages = core.SanitizeToolHistory(messages)
	inputLimit := config.InputLimit
	if inputLimit <= 0 {
		inputLimit = e.inputLimitForModel(model)
		config.InputLimit = inputLimit
	}
	prepared := e.PrepareCompactionContext(messages, config, "")

	reqMessages := buildHelperRequestMessages(
		systemPrompt,
		prepareMessagesForSummaryHelper(prepared.SummaryMessages),
		inputLimit,
		"I'll now summarize our conversation.",
		core.UserText(CompactionPrompt),
	)

	budget := config.SummaryTokenBudget
	if budget <= 0 {
		budget = defaultSummaryTokenBudget
	}
	// Centralized output clamping: cap to model's max output limit so providers
	// with small output limits don't reject the request. Providers also clamp
	// individually, but doing it here provides belt-and-suspenders safety.
	budget = e.clampOutputTokens(budget, model)

	// Call provider with no tools, enforcing output budget. Use an isolated
	// provider instance so the helper turn doesn't overwrite live server-side
	// conversation state (for example previous_response_id on Responses API clients).
	stream, err := e.isolatedProvider(provider).Stream(ctx, core.Request{
		Model:           model,
		Messages:        reqMessages,
		MaxOutputTokens: budget,
		MaxTurns:        1,
		Ephemeral:       true,
	})
	if err != nil {
		return nil, fmt.Errorf("compaction stream failed: %w", err)
	}
	defer stream.Close()

	collected, err := core.CollectTextStream(stream, nil)
	if err != nil {
		return nil, fmt.Errorf("compaction recv failed: %w", err)
	}

	briefText := continuationBriefFromTextAndReasoning(collected.Text, collected.ReasoningSummary)
	if briefText == "" {
		return nil, fmt.Errorf("compaction produced empty brief")
	}

	// Reconstruct history with the same tagged summary shape used by soft
	// compaction: deterministic previous-turn excerpts, the model-written
	// continuation brief, then a bounded raw suffix.
	result := e.CompactionResultFromBriefPrepared(systemPrompt, briefText, prepared, originalCount, config)
	result.Model = strings.TrimSpace(model)
	result.Usage = collected.Usage
	return result, nil
}

// HandoverResult describes the outcome of a handover compression.
type HandoverResult struct {
	Document    string         // The handover document text
	NewMessages []core.Message // [system] + [handover doc as user] + [assistant ack]
	SourceAgent string
	TargetAgent string
	Model       string
	Usage       core.Usage
}

// HandoverOptions controls whether generation may branch from live provider
// state. Callers must only enable forking at a settled provider boundary.
type HandoverOptions struct {
	AllowProviderFork bool
}

const handoverPromptTemplate = `You are handing over this conversation to a different agent (%s -> %s). Create a structured handover briefing that the new agent can use to continue the work.

Your handover document must include:
1. **Objective** — what the user is trying to accomplish
2. **Work Completed** — what was explored, discussed, and decided (chronologically)
3. **Current State** — exact files involved, errors found, test results, what's in progress
4. **Pending Tasks** — what still needs to be done, in priority order
5. **Key Context** — file paths, function names, config values, constraints, user preferences

Be specific and concrete — include exact file paths, function names, error messages, and code snippets when relevant. The new agent has no other context beyond this document.

Aim for roughly 800–1200 words. Prioritize exact current state, pending work, file paths, function names, errors, test results, and constraints. Remove narrative detail before removing facts needed to continue.`

// HandoverTriggerPrompt is the user turn that requests a handover briefing.
const HandoverTriggerPrompt = "Create the handover briefing now."

// handoverPrefix is prepended to the handover document in the reconstructed message history.
func handoverPrefix(source, target string) string {
	return fmt.Sprintf("[Agent Handover: @%s -> @%s]\n\n", source, target)
}

// ParseHandoverDocument separates a reconstructed briefing from its provenance.
// Only the exact prefix emitted by ReconstructHandoverHistory is accepted.
func ParseHandoverDocument(text string) (source, target, body string, ok bool) {
	line, body, found := strings.Cut(text, "\n\n")
	if !found || !strings.HasPrefix(line, "[Agent Handover: @") || !strings.HasSuffix(line, "]") {
		return "", "", "", false
	}
	names := strings.TrimSuffix(strings.TrimPrefix(line, "[Agent Handover: @"), "]")
	source, target, found = strings.Cut(names, " -> @")
	if !found || source == "" || target == "" || strings.ContainsAny(source+target, "\r\n") || strings.Contains(target, " -> @") {
		return "", "", "", false
	}
	return source, target, body, true
}

func handoverAcknowledgement(source string) string {
	return fmt.Sprintf("I've reviewed the handover briefing from @%s. I'll continue from where they left off.", source)
}

// IsHandoverAcknowledgement recognizes the synthetic acknowledgement, not model output.
func IsHandoverAcknowledgement(text string) bool {
	const prefix = "I've reviewed the handover briefing from @"
	const suffix = ". I'll continue from where they left off."
	source := strings.TrimSuffix(strings.TrimPrefix(text, prefix), suffix)
	return source != "" && !strings.ContainsAny(source, "\r\n") && text == handoverAcknowledgement(source)
}

func conversationEndsAtSettledAssistant(messages []core.Message) bool {
	last := len(messages) - 1
	for last >= 0 && messages[last].Role == core.RoleEvent {
		last--
	}
	if last < 0 || messages[last].Role != core.RoleAssistant {
		return false
	}
	pending := make(map[string]struct{})
	// Only unresolved calls in the current user turn can make the live provider
	// boundary unsafe. An abandoned call in older local history cannot still be
	// pending if the provider subsequently accepted another user turn.
	start := 0
	for i := last; i >= 0; i-- {
		if messages[i].Role == core.RoleUser {
			start = i
			break
		}
	}
	for _, message := range messages[start : last+1] {
		for _, part := range message.Parts {
			if part.ToolCall != nil {
				if id := strings.TrimSpace(part.ToolCall.ID); id != "" {
					pending[id] = struct{}{}
				}
			}
			if part.ToolResult != nil {
				delete(pending, strings.TrimSpace(part.ToolResult.ID))
			}
		}
	}
	return len(pending) == 0
}

// Handover generates a handover document from the conversation history using
// the outgoing provider. This is the Tier 2 fallback used when file-based
// handover is not available. The result contains reconstructed messages suitable
// for the new agent: [new system prompt] + [handover doc] + [assistant ack].
func (e CompactionEnv) Handover(ctx context.Context, provider core.Provider, model, currentSystemPrompt, newSystemPrompt string, messages []core.Message, sourceAgent, targetAgent string, config CompactionConfig, options HandoverOptions) (*HandoverResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to hand over")
	}

	settledBoundary := conversationEndsAtSettledAssistant(messages)
	messages = core.SanitizeToolHistory(messages)

	prompt := fmt.Sprintf(handoverPromptTemplate, sourceAgent, targetAgent)
	policy := core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: prompt}}}
	trigger := core.UserText(HandoverTriggerPrompt)

	// A settled assistant response is the exact provider boundary from which a
	// handover can branch. Reuse that server-side state when the provider supports
	// an isolated native fork; otherwise preserve the established full-history path.
	var handoverProvider core.Provider
	forked := options.AllowProviderFork && settledBoundary
	if forked {
		handoverProvider, forked = e.forkProvider(provider)
	}

	var reqMessages []core.Message
	if forked {
		// Claude CLI consumes this out-of-band as --system-prompt. Responses
		// continuation deliberately filters it and sends only policy + trigger.
		if currentSystemPrompt != "" {
			reqMessages = append(reqMessages, core.SystemText(currentSystemPrompt))
		}
		reqMessages = append(reqMessages, policy, trigger)
	} else {
		handoverProvider = e.isolatedProvider(provider)
		inputLimit := config.InputLimit
		if inputLimit <= 0 {
			inputLimit = e.inputLimitForModel(model)
		}
		reqMessages = buildHelperRequestMessages(
			currentSystemPrompt,
			messages,
			inputLimit,
			"I'll now prepare the handover briefing.",
			policy,
			trigger,
		)
	}

	budget := 12_000 // Slightly larger budget than compaction for handover precision
	budget = e.clampOutputTokens(budget, model)

	stream, err := handoverProvider.Stream(ctx, core.Request{
		Model:                          model,
		Messages:                       reqMessages,
		MaxOutputTokens:                budget,
		MaxTurns:                       1,
		Ephemeral:                      !forked,
		IncludeDeveloperInContinuation: forked,
	})
	if err != nil {
		return nil, fmt.Errorf("handover stream failed: %w", err)
	}
	defer stream.Close()

	collected, err := core.CollectTextStream(stream, nil)
	if err != nil {
		return nil, fmt.Errorf("handover recv failed: %w", err)
	}
	if strings.TrimSpace(collected.Text) == "" {
		return nil, fmt.Errorf("handover produced empty document")
	}

	// Reconstruct messages for the new agent with the new system prompt while
	// retaining the immutable conversation-start anchor across a model handover.
	newMessages := ReconstructHandoverHistory(newSystemPrompt, collected.Text, sourceAgent, targetAgent)
	newMessages = e.insertConversationStart(newMessages, messages)

	return &HandoverResult{
		Document:    collected.Text,
		NewMessages: newMessages,
		SourceAgent: sourceAgent,
		TargetAgent: targetAgent,
		Model:       strings.TrimSpace(model),
		Usage:       collected.Usage,
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
func ReconstructHandoverHistory(systemPrompt, document, sourceAgent, targetAgent string) []core.Message {
	var messages []core.Message

	if systemPrompt != "" {
		messages = append(messages, core.SystemText(systemPrompt))
	}

	prefix := handoverPrefix(sourceAgent, targetAgent)
	messages = append(messages, core.Message{
		Role:        core.RoleUser,
		Parts:       []core.Part{{Type: core.PartText, Text: prefix + document}},
		CacheAnchor: true,
	})

	ack := handoverAcknowledgement(sourceAgent)
	messages = append(messages, core.AssistantText(ack))

	return messages
}

// trimMessagesToFit removes messages from the front (after any system message)
// until the total estimated tokens fit within maxTokens. Preserves the system
// message, cache-anchored messages (previous compaction summaries), and the
// most recent messages. Ensures the result starts with a system or user message
// (not assistant/tool). Falls back to truncating oversized single messages.
func trimMessagesToFit(messages []core.Message, maxTokens int) []core.Message {
	if EstimateMessageTokens(messages) <= maxTokens {
		return messages
	}

	// Separate system prefix from conversation messages.
	startIdx := 0
	if len(messages) > 0 && messages[0].Role == core.RoleSystem {
		startIdx = 1
	}

	// Check for cache-anchored block after system (previous compaction summary
	// and its assistant ack). Preserving this during re-compaction retains
	// context from earlier compaction rounds.
	anchorEnd := startIdx
	if anchorEnd < len(messages) && messages[anchorEnd].CacheAnchor {
		anchorEnd++
		// Include the following assistant ack to maintain valid role alternation.
		if anchorEnd < len(messages) && messages[anchorEnd].Role == core.RoleAssistant {
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
func doTrim(messages []core.Message, preserveEnd, maxTokens int) []core.Message {
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
				conv = []core.Message{truncateMessageParts(conv[0], maxChars)}
			}
		}
	}

	// Ensure we start with a user message (providers reject leading assistant/tool).
	for len(conv) > 0 && conv[0].Role != core.RoleUser {
		conv = conv[1:]
	}

	result := make([]core.Message, 0, preserveEnd+len(conv))
	result = append(result, prefix...)
	result = append(result, conv...)
	return result
}

// truncateMessageParts truncates text and tool result content in a message
// to fit within maxChars total. Used when a single oversized message exceeds
// the compaction input budget.
func truncateMessageParts(msg core.Message, maxChars int) core.Message {
	result := core.Message{
		Role:        msg.Role,
		CacheAnchor: msg.CacheAnchor,
		Parts:       make([]core.Part, len(msg.Parts)),
	}
	copy(result.Parts, msg.Parts)
	for i, part := range result.Parts {
		if part.Text != "" {
			result.Parts[i].Text = TruncateToolResult(part.Text, maxChars)
		}
		if part.ToolResult != nil {
			tr := core.TruncateToolResultValue(*part.ToolResult, maxChars)
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
func reconstructHistory(systemPrompt, summary string, recentMsgs []core.Message) []core.Message {
	var messages []core.Message

	if systemPrompt != "" {
		messages = append(messages, core.SystemText(systemPrompt))
	}

	// The summary message gets a cache anchor so the Anthropic provider applies
	// cache_control: ephemeral to it. The summary is stable until the next
	// compaction, so caching it here gives a cheap warm hit on every subsequent turn.
	messages = append(messages, core.Message{
		Role:        core.RoleUser,
		Parts:       []core.Part{{Type: core.PartText, Text: SummaryPrefix + summary}},
		CacheAnchor: true,
	})

	// Add an assistant acknowledgement unless the retained raw suffix already
	// starts with an assistant message. This avoids consecutive assistant turns in
	// the common split-suffix case while still preventing summary-user + recent-user
	// from being interpreted as a brand-new user request pair.
	if len(recentMsgs) == 0 || recentMsgs[0].Role != core.RoleAssistant {
		messages = append(messages, core.AssistantText(CompactionAckText))
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

// IsContextOverflowError checks whether an error indicates that the context
// window was exceeded. Checks error strings across providers.
func IsContextOverflowError(err error) bool {
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

func compactMediaSummary(media []core.MediaArtifact) []string {
	counts := map[string]int{}
	for _, item := range media {
		kind := strings.SplitN(strings.ToLower(item.MediaType), "/", 2)[0]
		if kind == "image" || kind == "video" || kind == "audio" {
			counts[kind]++
		}
	}
	var details []string
	for _, kind := range []string{"image", "video", "audio"} {
		if counts[kind] > 0 {
			details = append(details, fmt.Sprintf("%d %s(s)", counts[kind], kind))
		}
	}
	return details
}
