package core

// ReasoningDisplay is one ordered, display-only reasoning burst. Unlike the
// aggregate Part.Reasoning* fields, it is never echoed to a provider and cannot
// contain encrypted replay state. Consumers must still apply their source and
// persistence policies before rendering or storing its content.
type ReasoningDisplay struct {
	Content      string
	SummaryParts []string `json:",omitempty"`
	Kind         ReasoningKind
	SummaryTitle string `json:",omitempty"`
}

// ReasoningForDisplay selects an ordered burst over aggregate replay metadata,
// falling back to legacy persisted reasoning when no burst representation exists.
// A replay-only marker must survive display-policy filtering so that removing a
// burst never makes the aggregate visible again. The result is read-only.
func ReasoningForDisplay(part Part) *ReasoningDisplay {
	if part.ReasoningDisplay != nil {
		return part.ReasoningDisplay
	}
	if part.ReasoningReplayOnly || (part.ReasoningContent == "" && len(part.ReasoningSummaryParts) == 0) {
		return nil
	}
	return &ReasoningDisplay{
		Content:      part.ReasoningContent,
		SummaryParts: part.ReasoningSummaryParts,
		Kind:         NormalizeStoredReasoningKind(part.ReasoningKind, true),
		SummaryTitle: part.ReasoningSummaryTitle,
	}
}
