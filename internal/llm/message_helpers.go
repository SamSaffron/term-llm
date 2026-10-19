package llm

import "github.com/samsaffron/term-llm/internal/llm/core"

func hasReasoningMetadata(part core.Part) bool {
	return part.ReasoningContent != "" || part.ReasoningKind != "" || part.ReasoningSummaryTitle != "" || part.ReasoningItemID != "" || part.ReasoningEncryptedContent != ""
}
