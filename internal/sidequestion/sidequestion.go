package sidequestion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	HistoryLimit = 20

	// sideInputBudgetRatio leaves room for provider serialization overhead and
	// estimation error. InputLimitForProviderModel is already the input-only
	// limit, so this is safety headroom rather than an output-token reservation.
	sideInputBudgetRatio = 0.80
	// Unknown/custom models still need a bounded request. This conservative
	// fallback avoids reverting to the previous unbounded behavior when model
	// metadata is unavailable.
	fallbackSideInputLimit = 8_000

	SystemPolicy = `This is a private side question about the current conversation.
Answer directly in one response, with enough detail to answer the question well.
Use clear Markdown structure when it improves readability, including short headings, bullets, tables, or code blocks where appropriate.
For a simple question, give a simple answer; do not add ornament, sections, or verbosity that do not help.
The main conversation continues independently.
Use only the supplied conversation and side-question history.
You have no tools and cannot inspect files, run commands, search, delegate, or take actions.
If the answer is not available in the provided context, say so.
Do not promise future action.`
	ToolAttemptResponse = "Side questions cannot use tools. Ask using only the supplied conversation context."
)

type Entry struct {
	Question  string    `json:"question"`
	Response  string    `json:"response"`
	CreatedAt time.Time `json:"created_at"`
	Usage     llm.Usage `json:"usage"`
}

type Result struct {
	Response  string    `json:"response"`
	Usage     llm.Usage `json:"usage"`
	Synthetic bool      `json:"synthetic"`
}

// PrepareContextSnapshot returns a provider-safe point-in-time deep copy. It
// preserves cache anchors while removing incomplete tool protocol fragments.
func PrepareContextSnapshot(messages []llm.Message) []llm.Message {
	copied := deepCopyMessages(messages)
	type position struct{ message, part int }
	pending := make(map[string]position)
	seen := make(map[string]struct{})
	complete := make(map[position]struct{})
	for messageIndex, msg := range copied {
		for partIndex, part := range msg.Parts {
			pos := position{messageIndex, partIndex}
			switch {
			case part.ToolCall != nil:
				id := strings.TrimSpace(part.ToolCall.ID)
				if id == "" {
					continue
				}
				if _, duplicate := seen[id]; duplicate {
					continue
				}
				seen[id] = struct{}{}
				pending[id] = pos
			case part.ToolResult != nil:
				id := strings.TrimSpace(part.ToolResult.ID)
				callPos, ok := pending[id]
				if !ok {
					continue
				}
				complete[callPos] = struct{}{}
				complete[pos] = struct{}{}
				delete(pending, id)
			}
		}
	}
	out := make([]llm.Message, 0, len(copied))
	for messageIndex, msg := range copied {
		if msg.Role == llm.RoleEvent {
			continue
		}
		partial := false
		parts := make([]llm.Part, 0, len(msg.Parts))
		for partIndex, part := range msg.Parts {
			if part.ToolCall != nil || part.ToolResult != nil {
				if _, ok := complete[position{messageIndex, partIndex}]; !ok {
					partial = true
					continue
				}
			}
			parts = append(parts, part)
		}
		if partial {
			filtered := parts[:0]
			for _, part := range parts {
				if part.Type != llm.PartProviderReplay {
					filtered = append(filtered, part)
				}
			}
			parts = filtered
		}
		msg.Parts = parts
		if len(parts) > 0 {
			out = append(out, msg)
		}
	}
	return out
}

// CloneMessages returns a point-in-time deep copy without changing provider
// protocol metadata. Request construction performs sanitization once after all
// context fragments have been assembled.
func CloneMessages(messages []llm.Message) []llm.Message {
	return deepCopyMessages(messages)
}

func deepCopyMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	for i, msg := range messages {
		out[i] = msg
		out[i].Parts = append([]llm.Part(nil), msg.Parts...)
		for j := range out[i].Parts {
			part := &out[i].Parts[j]
			part.ReasoningSummaryParts = append([]string(nil), part.ReasoningSummaryParts...)
			if part.ImageData != nil {
				image := *part.ImageData
				part.ImageData = &image
			}
			if part.FileData != nil {
				file := *part.FileData
				part.FileData = &file
			}
			if part.ProviderReplay != nil {
				replay := *part.ProviderReplay
				replay.Raw = append([]byte(nil), replay.Raw...)
				part.ProviderReplay = &replay
			}
			if part.ToolCall != nil {
				call := *part.ToolCall
				call.Arguments = append([]byte(nil), call.Arguments...)
				call.ThoughtSig = append([]byte(nil), call.ThoughtSig...)
				part.ToolCall = &call
			}
			if part.ToolResult != nil {
				result := *part.ToolResult
				result.ContentParts = append([]llm.ToolContentPart(nil), result.ContentParts...)
				for k := range result.ContentParts {
					if result.ContentParts[k].ImageData != nil {
						image := *result.ContentParts[k].ImageData
						result.ContentParts[k].ImageData = &image
					}
				}
				result.Diffs = append([]llm.DiffData(nil), result.Diffs...)
				result.Images = append([]string(nil), result.Images...)
				result.Media = append([]llm.MediaArtifact(nil), result.Media...)
				result.ThoughtSig = append([]byte(nil), result.ThoughtSig...)
				part.ToolResult = &result
			}
		}
	}
	return out
}

// BuildMessages constructs a bounded provider request. It preserves the exact
// main-session prefix whenever possible for prompt-cache reuse, then drops old
// side history before trimming main history at complete user-turn boundaries.
func BuildMessages(snapshot []llm.Message, history []Entry, question, providerName, model string, runtimeInputLimit int) ([]llm.Message, error) {
	inputLimit := runtimeInputLimit
	if inputLimit <= 0 {
		inputLimit = llm.InputLimitForProviderModel(providerName, model)
	}
	if inputLimit <= 0 {
		inputLimit = fallbackSideInputLimit
	}
	budget := int(float64(inputLimit) * sideInputBudgetRatio)
	return buildMessagesWithinBudget(snapshot, history, question, budget)
}

func buildMessagesWithinBudget(snapshot []llm.Message, history []Entry, question string, budget int) ([]llm.Message, error) {
	if budget <= 0 {
		return nil, errors.New("cannot safely build side question: input token budget is unavailable")
	}

	snapshot = PrepareContextSnapshot(snapshot)
	if len(history) > HistoryLimit {
		history = history[len(history)-HistoryLimit:]
	}
	policy := llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: SystemPolicy}}}
	questionMessage := llm.UserText(strings.TrimSpace(question))
	required := []llm.Message{policy, questionMessage}
	if estimateSideMessageTokens(required) > budget {
		questionMessage = llm.UserText("")
		required[1] = questionMessage
		available := budget - estimateSideMessageTokens(required)
		if available <= 0 {
			return nil, fmt.Errorf("cannot safely build side question: policy and request framing exceed the %d-token budget", budget)
		}
		questionMessage = llm.UserText(truncateTextToTokens(strings.TrimSpace(question), available))
		required[1] = questionMessage
	}

	sideMessages := make([]llm.Message, 0, len(history)*2)
	for _, entry := range history {
		sideMessages = append(sideMessages, llm.UserText(entry.Question), llm.AssistantText(entry.Response))
	}

	snapshotTokens := estimateSideMessageTokens(snapshot)
	sideTokens := estimateSideMessageTokens(sideMessages)
	requiredTokens := estimateSideMessageTokens(required)
	if snapshotTokens+sideTokens+requiredTokens <= budget {
		return joinMessages(snapshot, sideMessages, required), nil
	}

	// Side history is optional. Remove its oldest exchanges first so the exact
	// main snapshot remains the request prefix (and remains cache-reusable).
	for len(sideMessages) > 0 {
		sideTokens -= estimateSideMessageTokens(sideMessages[:2])
		sideMessages = sideMessages[2:]
		if snapshotTokens+sideTokens+requiredTokens <= budget {
			return joinMessages(snapshot, sideMessages, required), nil
		}
	}

	// Preserve stable instructions and a compaction anchor/ack when one begins
	// the conversation, then retain the largest recent suffix beginning at a
	// user turn. Re-sanitize each selected main-context candidate so unusual
	// user interjections cannot leave an orphaned tool result after the cut.
	prefixEnd := stableMainPrefixEnd(snapshot)
	prefix := snapshot[:prefixEnd]
	messageTokens := make([]int, len(snapshot))
	for i := range snapshot {
		messageTokens[i] = estimateSideMessageTokens(snapshot[i : i+1])
	}
	suffixTokens := make([]int, len(snapshot)+1)
	for i := len(snapshot) - 1; i >= 0; i-- {
		suffixTokens[i] = suffixTokens[i+1] + messageTokens[i]
	}
	prefixTokens := suffixTokens[0] - suffixTokens[prefixEnd]
	for start := prefixEnd; start < len(snapshot); start++ {
		if snapshot[start].Role != llm.RoleUser || prefixTokens+suffixTokens[start]+requiredTokens > budget {
			continue
		}
		if candidate, ok := fitTrimCandidate(joinMessages(prefix, snapshot[start:]), required, budget); ok {
			return candidate, nil
		}
	}

	lastUser := lastUserMessageIndex(snapshot, prefixEnd)
	if lastUser >= 0 {
		if candidate, ok := truncateMainCandidate(joinMessages(prefix, snapshot[lastUser:]), required, budget); ok {
			return candidate, nil
		}
	}
	if candidate, ok := fitTrimCandidate(prefix, required, budget); ok {
		return candidate, nil
	}

	// If even the stable prefix is oversized, favor recent coherent context over
	// failing the provider request. This is the only path that sacrifices all
	// parent-prefix cache reuse.
	for start := 0; start < len(snapshot); start++ {
		if snapshot[start].Role != llm.RoleUser || suffixTokens[start]+requiredTokens > budget {
			continue
		}
		if candidate, ok := fitTrimCandidate(snapshot[start:], required, budget); ok {
			return candidate, nil
		}
	}
	if lastUser = lastUserMessageIndex(snapshot, 0); lastUser >= 0 {
		if candidate, ok := truncateMainCandidate(snapshot[lastUser:], required, budget); ok {
			return candidate, nil
		}
	}
	if requiredTokens > budget {
		return nil, fmt.Errorf("cannot safely build side question within %d-token budget", budget)
	}
	return required, nil
}

func fitTrimCandidate(main, required []llm.Message, budget int) ([]llm.Message, bool) {
	main = PrepareContextSnapshot(main)
	candidate := joinMessages(main, required)
	return candidate, estimateSideMessageTokens(candidate) <= budget
}

func lastUserMessageIndex(messages []llm.Message, start int) int {
	for i := len(messages) - 1; i >= start; i-- {
		if messages[i].Role == llm.RoleUser {
			return i
		}
	}
	return -1
}

func truncateMainCandidate(main, required []llm.Message, budget int) ([]llm.Message, bool) {
	main = PrepareContextSnapshot(main)
	if len(main) == 0 || estimateSideMessageTokens(required) >= budget {
		return nil, false
	}
	for maxChars := budget * 4; maxChars >= 64; maxChars /= 2 {
		trimmed := truncateConversationPayloads(main, maxChars)
		candidate := joinMessages(trimmed, required)
		if estimateSideMessageTokens(candidate) <= budget {
			return candidate, true
		}
	}
	return nil, false
}

func truncateConversationPayloads(messages []llm.Message, maxChars int) []llm.Message {
	messages = CloneMessages(messages)
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		parts := make([]llm.Part, 0, len(message.Parts))
		for _, part := range message.Parts {
			if part.ProviderReplay != nil || part.Type == llm.PartProviderReplay {
				continue
			}
			part.Text = truncateTextToChars(part.Text, maxChars)
			part.ReasoningContent = truncateTextToChars(part.ReasoningContent, maxChars)
			part.ReasoningSummaryParts = nil
			part.ReasoningEncryptedContent = ""
			part.ImageData = nil
			part.ImagePath = ""
			part.FileData = nil
			part.FilePath = ""
			if part.ToolResult != nil {
				result := *part.ToolResult
				result.Content = truncateTextToChars(result.Content, maxChars)
				result.Images = nil
				result.Media = nil
				result.ContentParts = append([]llm.ToolContentPart(nil), result.ContentParts...)
				for i := range result.ContentParts {
					result.ContentParts[i].Text = truncateTextToChars(result.ContentParts[i].Text, maxChars)
					result.ContentParts[i].ImageData = nil
				}
				part.ToolResult = &result
			}
			parts = append(parts, part)
		}
		message.Parts = parts
		if len(parts) > 0 {
			out = append(out, message)
		}
	}
	return PrepareContextSnapshot(out)
}

func truncateTextToChars(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	cut := maxChars
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut])
}

func stableMainPrefixEnd(messages []llm.Message) int {
	end := 0
	for end < len(messages) && (messages[end].Role == llm.RoleSystem || messages[end].Role == llm.RoleDeveloper) {
		end++
	}
	if end < len(messages) && messages[end].CacheAnchor {
		end++
		if end < len(messages) && messages[end].Role == llm.RoleAssistant {
			end++
		}
	}
	return end
}

func joinMessages(groups ...[]llm.Message) []llm.Message {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	messages := make([]llm.Message, 0, total)
	for _, group := range groups {
		messages = append(messages, group...)
	}
	return messages
}

func estimateSideMessageTokens(messages []llm.Message) int {
	// The shared estimator covers model-visible text/reasoning/tool payloads.
	// Side snapshots can also contain opaque provider replay and multimodal data;
	// count their serialized bytes conservatively so they cannot bypass fitting.
	total := llm.EstimateMessageTokens(messages)
	for _, message := range messages {
		total += 4 // role and message envelope
		for _, part := range message.Parts {
			total += 4 // content-part envelope
			if part.ProviderReplay != nil {
				total += llm.EstimateTokens(string(part.ProviderReplay.Raw))
			}
			if part.ImageData != nil {
				total += llm.EstimateTokens(part.ImageData.Base64)
			}
			if part.ImagePath != "" {
				total += estimateFilePathTokens(part.ImagePath, 4)
			}
			if part.FileData != nil {
				total += llm.EstimateTokens(part.FileData.Base64)
				if part.FileData.Base64 == "" && part.FileData.SizeBytes > 0 {
					total += int(part.FileData.SizeBytes+2) / 3
				}
			}
			if part.FilePath != "" {
				total += estimateFilePathTokens(part.FilePath, 3)
			}
			if part.ToolResult != nil {
				for _, contentPart := range part.ToolResult.ContentParts {
					total += llm.EstimateTokens(contentPart.Text)
					if contentPart.ImageData != nil {
						total += llm.EstimateTokens(contentPart.ImageData.Base64)
					}
				}
			}
		}
	}
	return total
}

func estimateFilePathTokens(path string, bytesPerToken int64) int {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 0 {
		return 0
	}
	return int((info.Size() + bytesPerToken - 1) / bytesPerToken)
}

func truncateTextToTokens(text string, maxTokens int) string {
	maxBytes := maxTokens * 4
	if len(text) <= maxBytes {
		return text
	}
	for maxBytes > 0 && !utf8.ValidString(text[:maxBytes]) {
		maxBytes--
	}
	return strings.TrimSpace(text[:maxBytes])
}

func AppendHistory(history []Entry, entry Entry) []Entry {
	if strings.TrimSpace(entry.Response) == "" {
		return history
	}
	history = append(history, entry)
	if len(history) > HistoryLimit {
		history = append([]Entry(nil), history[len(history)-HistoryLimit:]...)
	}
	return history
}

// Run performs exactly one provider request. It bypasses the agent engine so no
// local, MCP, approval, delegation, or provider-native search capability exists.
func Run(ctx context.Context, provider llm.Provider, req llm.Request, emit func(llm.Event)) (Result, error) {
	req.Ephemeral = true
	req.SessionID = ""
	req.Tools = nil
	req.ToolMap = nil
	req.ToolChoice = llm.ToolChoice{}
	req.LastTurnToolChoice = nil
	req.ParallelToolCalls = false
	req.Search = false
	req.ForceExternalSearch = false
	req.DisableExternalWebFetch = true
	req.MaxTurns = 1
	req.Responses = &llm.ResponsesOptions{
		ReasoningMode:           reqReasoningMode(req),
		MultiAgent:              llm.MultiAgentOptions{Enabled: false, EnabledSet: true},
		ProgrammaticToolCalling: llm.ProgrammaticToolCallingOptions{Enabled: false, EnabledSet: true},
	}
	stream, err := provider.Stream(ctx, req)
	if err != nil {
		return Result{}, err
	}
	defer stream.Close()

	errToolAttempt := errors.New("side question attempted tool use")
	collected, err := llm.CollectTextStream(stream, func(event llm.Event) error {
		switch event.Type {
		case llm.EventToolCall, llm.EventToolExecStart, llm.EventToolExecEnd:
			if emit != nil {
				emit(llm.Event{Type: llm.EventTextDelta, Text: ToolAttemptResponse})
			}
			return errToolAttempt
		default:
			if emit != nil {
				emit(event)
			}
			return nil
		}
	})
	if errors.Is(err, errToolAttempt) {
		return Result{Response: ToolAttemptResponse, Usage: collected.Usage, Synthetic: true}, nil
	}
	if err != nil {
		return Result{Response: collected.Text, Usage: collected.Usage}, err
	}
	return Result{Response: strings.TrimSpace(collected.Text), Usage: collected.Usage}, nil
}

func reqReasoningMode(req llm.Request) string {
	if req.Responses != nil {
		return req.Responses.ReasoningMode
	}
	return ""
}
