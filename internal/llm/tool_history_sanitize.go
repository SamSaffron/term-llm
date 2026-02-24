package llm

import (
	"fmt"
	"strings"
)

type toolCallRef struct {
	messageIndex int
	partIndex    int
}

// sanitizeToolHistory removes dangling tool calls and orphan tool results.
// It preserves non-tool content while enforcing call/result pair integrity.
func sanitizeToolHistory(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}

	sanitized := make([]Message, 0, len(messages))
	pendingCalls := make(map[string][]toolCallRef)
	matchedCalls := make(map[int]map[int]bool)

	for _, msg := range messages {
		switch msg.Role {
		case RoleAssistant:
			assistantIndex := len(sanitized)
			parts := make([]Part, 0, len(msg.Parts))

			for _, part := range msg.Parts {
				cloned, ok := clonePart(part)
				if !ok {
					continue
				}

				if cloned.Type == PartToolCall {
					callID := ""
					if cloned.ToolCall != nil {
						callID = strings.TrimSpace(cloned.ToolCall.ID)
					}
					if callID == "" {
						continue
					}
					partIndex := len(parts)
					parts = append(parts, cloned)
					pendingCalls[callID] = append(pendingCalls[callID], toolCallRef{
						messageIndex: assistantIndex,
						partIndex:    partIndex,
					})
					continue
				}

				parts = append(parts, cloned)
			}

			if len(parts) > 0 {
				sanitized = append(sanitized, Message{Role: msg.Role, Parts: parts})
			}

		case RoleTool:
			parts := make([]Part, 0, len(msg.Parts))

			for _, part := range msg.Parts {
				cloned, ok := clonePart(part)
				if !ok {
					continue
				}

				if cloned.Type != PartToolResult {
					parts = append(parts, cloned)
					continue
				}

				resultID := ""
				if cloned.ToolResult != nil {
					resultID = strings.TrimSpace(cloned.ToolResult.ID)
				}
				if resultID == "" {
					continue
				}

				refs := pendingCalls[resultID]
				if len(refs) == 0 {
					continue
				}

				ref := refs[0]
				if len(refs) == 1 {
					delete(pendingCalls, resultID)
				} else {
					pendingCalls[resultID] = refs[1:]
				}

				if matchedCalls[ref.messageIndex] == nil {
					matchedCalls[ref.messageIndex] = make(map[int]bool)
				}
				matchedCalls[ref.messageIndex][ref.partIndex] = true

				parts = append(parts, cloned)
			}

			if len(parts) > 0 {
				sanitized = append(sanitized, Message{Role: msg.Role, Parts: parts})
			}

		default:
			sanitized = append(sanitized, Message{
				Role:  msg.Role,
				Parts: cloneParts(msg.Parts),
			})
		}
	}

	finalMessages := make([]Message, 0, len(sanitized))
	for msgIndex, msg := range sanitized {
		if msg.Role != RoleAssistant {
			finalMessages = append(finalMessages, msg)
			continue
		}

		matches := matchedCalls[msgIndex]
		parts := make([]Part, 0, len(msg.Parts))
		for partIndex, part := range msg.Parts {
			if part.Type == PartToolCall {
				if matches == nil || !matches[partIndex] {
					// Orphaned tool call — no matching tool_result was found (e.g. compaction
					// trimmed the result). Convert to text so the model knows what it attempted
					// rather than silently dropping it, which causes 400s on Anthropic.
					if part.ToolCall != nil {
						text := fmt.Sprintf("[tool call interrupted — id:%s name:%s args:%s]",
							part.ToolCall.ID, part.ToolCall.Name, string(part.ToolCall.Arguments))
						parts = append(parts, Part{Type: PartText, Text: text})
					}
					continue
				}
			}
			parts = append(parts, part)
		}

		if len(parts) > 0 {
			finalMessages = append(finalMessages, Message{
				Role:  msg.Role,
				Parts: parts,
			})
		}
	}

	return finalMessages
}

func cloneParts(parts []Part) []Part {
	cloned := make([]Part, 0, len(parts))
	for _, part := range parts {
		clone, ok := clonePart(part)
		if !ok {
			continue
		}
		cloned = append(cloned, clone)
	}
	return cloned
}

func clonePart(part Part) (Part, bool) {
	cloned := part

	switch part.Type {
	case PartImage:
		if part.ImageData != nil {
			imageCopy := *part.ImageData
			cloned.ImageData = &imageCopy
		}
	case PartToolCall:
		if part.ToolCall == nil {
			return Part{}, false
		}
		call := *part.ToolCall
		if len(call.Arguments) > 0 {
			call.Arguments = append([]byte(nil), call.Arguments...)
		}
		if len(call.ThoughtSig) > 0 {
			call.ThoughtSig = append([]byte(nil), call.ThoughtSig...)
		}
		cloned.ToolCall = &call

	case PartToolResult:
		if part.ToolResult == nil {
			return Part{}, false
		}
		result := *part.ToolResult
		if len(result.ContentParts) > 0 {
			result.ContentParts = cloneToolContentParts(result.ContentParts)
		}
		if len(result.Diffs) > 0 {
			result.Diffs = append([]DiffData(nil), result.Diffs...)
		}
		if len(result.Images) > 0 {
			result.Images = append([]string(nil), result.Images...)
		}
		if len(result.ThoughtSig) > 0 {
			result.ThoughtSig = append([]byte(nil), result.ThoughtSig...)
		}
		cloned.ToolResult = &result
	}

	return cloned, true
}

func cloneToolContentParts(parts []ToolContentPart) []ToolContentPart {
	cloned := make([]ToolContentPart, 0, len(parts))
	for _, part := range parts {
		copyPart := part
		if part.ImageData != nil {
			imageCopy := *part.ImageData
			copyPart.ImageData = &imageCopy
		}
		cloned = append(cloned, copyPart)
	}
	return cloned
}
