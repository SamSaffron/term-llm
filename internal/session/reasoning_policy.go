package session

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// ApplyReasoningPersistencePolicy strips disabled summaries without mutating msg.
// Provider replay IDs, encrypted payloads, and raw reasoning remain available for
// replay/debug; display and export are gated separately.
func ApplyReasoningPersistencePolicy(msg core.Message, cfg config.ReasoningConfig) core.Message {
	if cfg == (config.ReasoningConfig{}) {
		cfg = config.DefaultReasoningConfig()
	}
	if cfg.PersistSummaries {
		return msg
	}
	cloned := msg
	cloned.Parts = append([]core.Part(nil), msg.Parts...)
	for i := range cloned.Parts {
		if display := cloned.Parts[i].ReasoningDisplay; display != nil {
			if core.NormalizeStoredReasoningKind(display.Kind, display.Content != "" || len(display.SummaryParts) > 0) == core.ReasoningKindSummary {
				cloned.Parts[i].ReasoningDisplay = nil
				// Removing the burst must not expose cumulative replay metadata.
				cloned.Parts[i].ReasoningReplayOnly = true
			}
		}
		hasReasoningContent := cloned.Parts[i].ReasoningContent != "" || len(cloned.Parts[i].ReasoningSummaryParts) > 0
		if core.NormalizeStoredReasoningKind(cloned.Parts[i].ReasoningKind, hasReasoningContent) == core.ReasoningKindSummary {
			cloned.Parts[i].ReasoningContent = ""
			cloned.Parts[i].ReasoningSummaryParts = nil
			cloned.Parts[i].ReasoningSummaryTitle = ""
		}
	}
	return cloned
}

// NewMessageWithReasoningPolicy creates a session message after applying the
// configured reasoning persistence policy.
func NewMessageWithReasoningPolicy(sessionID string, msg core.Message, sequence int, cfg config.ReasoningConfig) *Message {
	return NewMessage(sessionID, ApplyReasoningPersistencePolicy(msg, cfg), sequence)
}
