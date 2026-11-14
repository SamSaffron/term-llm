package llm

import "github.com/term-llm/term-llm/internal/llm/core"

func hasReasoningMetadata(part core.Part) bool {
	return part.ReasoningContent != "" || part.ReasoningKind != "" || part.ReasoningSummaryTitle != "" || part.ReasoningItemID != "" || part.ReasoningEncryptedContent != ""
}
