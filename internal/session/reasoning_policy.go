package session

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

// ApplyReasoningPersistencePolicy removes display-only reasoning summary text
// when the user disabled summary persistence, while preserving provider replay
// identifiers/encrypted payloads. Raw reasoning is left unchanged because some
// providers use it as replay/debug metadata and display/export remain gated
// separately.
func ApplyReasoningPersistencePolicy(msg llm.Message, cfg config.ReasoningConfig) llm.Message {
	if cfg == (config.ReasoningConfig{}) {
		cfg = config.DefaultReasoningConfig()
	}
	if cfg.PersistSummaries {
		return msg
	}
	cloned := llm.Message{
		Role:        msg.Role,
		CacheAnchor: msg.CacheAnchor,
	}
	if len(msg.Parts) == 0 {
		return cloned
	}
	cloned.Parts = make([]llm.Part, len(msg.Parts))
	// Part structs are copied before mutating display-only reasoning fields. Nested
	// pointer/slice fields remain shared unless explicitly replaced below.
	copy(cloned.Parts, msg.Parts)
	for i := range cloned.Parts {
		hasReasoningContent := cloned.Parts[i].ReasoningContent != "" || len(cloned.Parts[i].ReasoningSummaryParts) > 0
		if llm.NormalizeStoredReasoningKind(cloned.Parts[i].ReasoningKind, hasReasoningContent) == llm.ReasoningKindSummary {
			cloned.Parts[i].ReasoningContent = ""
			cloned.Parts[i].ReasoningSummaryParts = nil
			cloned.Parts[i].ReasoningSummaryTitle = ""
		}
	}
	return cloned
}

// NewMessageWithReasoningPolicy creates a session message after applying the
// configured reasoning persistence policy.
func NewMessageWithReasoningPolicy(sessionID string, msg llm.Message, sequence int, cfg config.ReasoningConfig) *Message {
	return NewMessage(sessionID, ApplyReasoningPersistencePolicy(msg, cfg), sequence)
}
