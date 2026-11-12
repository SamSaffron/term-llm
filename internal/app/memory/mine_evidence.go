package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func excludedMemoryEvidenceRole(role core.Role) bool {
	return role == core.RoleSystem || role == core.RoleDeveloper || role == core.RoleEvent
}
func (opts MineOptions) loadMessagesForMining(ctx context.Context, store session.Store, candidate memoryMineCandidate, offset int, taxonomyMap string) (memoryMineLoadResult, error) {
	remaining := opts.MaxMessages
	currentSeq := offset
	all := make([]session.Message, 0, opts.BatchSize)
	result := memoryMineLoadResult{}

	for {
		limit := opts.BatchSize
		if remaining > 0 && remaining < limit {
			limit = remaining
		}

		msgs, err := store.GetMessagesFrom(ctx, candidate.Session.ID, currentSeq, limit)
		if err != nil {
			return result, err
		}
		if len(msgs) == 0 {
			break
		}

		for _, msg := range msgs {
			nextSeq := msg.Sequence + 1
			skip, stop, err := opts.messageScope(msg)
			if err != nil {
				return result, err
			}
			if stop {
				result.UpperBoundary = true
				result.Messages = all
				result.NextOffset = currentSeq
				return result, nil
			}
			if skip {
				result.SkippedBefore++
				currentSeq = nextSeq
				continue
			}
			if excludedMemoryEvidenceRole(msg.Role) {
				currentSeq = nextSeq
				continue
			}
			msg = protectionOrDefault(opts.protection).message(msg)
			candidateMessages := append(cloneMessages(all), msg)
			if opts.hasMessageBounds() && !opts.boundedPromptFits(candidate, offset, nextSeq, candidateMessages, taxonomyMap) {
				result.PromptBudgetHit = true
				result.Messages = all
				result.NextOffset = currentSeq
				if len(all) == 0 {
					return result, fmt.Errorf("message at sequence %d exceeds lossless prompt budget; increase --prompt-max-tokens; cursor retained", msg.Sequence)
				}
				return result, nil
			}
			fit, ok := transcriptFitResult{Messages: candidateMessages}, true
			if !opts.hasMessageBounds() {
				fit, ok = opts.fitMessagesForPromptBudget(candidate, offset, nextSeq, candidateMessages, taxonomyMap)
			}
			if !ok {
				result.PromptBudgetHit = true
				result.Messages = all
				result.NextOffset = currentSeq
				return result, nil
			}
			all = fit.Messages
			if opts.hasMessageBounds() {
				result.addEvidence(msg.Sequence)
			}
			currentSeq = nextSeq
			result.TruncatedMessages = fit.TruncatedMessages
			result.AssistantMessagesCut = fit.AssistantMessagesCut
			result.UserMessagesCut = fit.UserMessagesCut
			result.SingleMessageClipped = fit.SingleMessageClipped
			if fit.TruncatedMessages > 0 {
				result.PromptBudgetHit = true
			}
			if remaining > 0 {
				remaining--
				if remaining <= 0 {
					result.Messages = all
					result.NextOffset = currentSeq
					return result, nil
				}
			}
		}

		if len(msgs) < limit {
			break
		}
	}

	result.Messages = all
	result.NextOffset = currentSeq
	return result, nil
}

func buildExtractionPrompt(candidate memoryMineCandidate, startOffset, endOffset int, messages []session.Message, taxonomyMap string) string {
	return buildExtractionPromptParts(candidate, startOffset, endOffset, messages, taxonomyMap).Prompt
}

func buildTranscript(messages []session.Message) []transcriptMessage {
	out := make([]transcriptMessage, 0, len(messages))

	for _, msg := range messages {
		if excludedMemoryEvidenceRole(msg.Role) {
			continue
		}
		entry := transcriptMessage{Role: string(msg.Role)}

		if msg.Role != core.RoleTool {
			if text := strings.TrimSpace(msg.TextContent); text != "" {
				entry.Text = text
			}
		}

		calls := collectToolCallNames(msg)
		if len(calls) > 0 {
			entry.ToolCalls = calls
		}

		if entry.Text == "" && len(entry.ToolCalls) == 0 && msg.Role == core.RoleTool {
			// Explicitly suppress raw tool output.
			continue
		}

		out = append(out, entry)
	}

	return out
}
