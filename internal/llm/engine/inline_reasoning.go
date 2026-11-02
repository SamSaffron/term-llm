package engine

import (
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
)

// inlineReasoningState tracks display boundaries independently of cumulative
// provider replay fields. Summary snapshots may repeat text emitted before a
// tool, so only their not-yet-emitted suffix belongs in the next burst.
type inlineReasoningState struct {
	active      bool
	itemID      string
	itemContent string
	burstPrefix string
}

func (s *inlineReasoningState) append(parts *[]core.Part, event core.Event) {
	// Require a positive display classification before accumulating any text or
	// summary parts: a later safe delta must never promote an unknown payload.
	// Encrypted replay metadata may accompany a positively classified summary.
	kind := core.NormalizeReasoningKind(event.ReasoningKind)
	if kind == core.ReasoningKindUnknown || kind == core.ReasoningKindEncrypted || (event.Text == "" && len(event.ReasoningSummaryParts) == 0) {
		return
	}
	previousItemID := s.itemID
	newItem := event.ReasoningItemID != "" && event.ReasoningItemID != s.itemID
	if newItem {
		s.itemID = event.ReasoningItemID
		s.itemContent = ""
	}
	text := event.Text
	if text == "" && len(event.ReasoningSummaryParts) > 0 {
		summary := strings.Join(event.ReasoningSummaryParts, "\n\n")
		if strings.HasPrefix(summary, s.itemContent) {
			text = strings.TrimPrefix(summary, s.itemContent)
		}
	}
	if text == "" && !s.active {
		return
	}
	if !s.active {
		s.burstPrefix = s.itemContent
		*parts = append(*parts, core.Part{Type: core.PartText, CreatedAt: time.Now().UnixMilli(), ReasoningDisplay: &core.ReasoningDisplay{}})
		s.active = true
	}
	part := &(*parts)[len(*parts)-1]
	// Replace rather than mutate: already-published snapshots remain immutable.
	display := *part.ReasoningDisplay
	if newItem && display.Content != "" && text != "" {
		var builder strings.Builder
		builder.WriteString(display.Content)
		internalreasoning.AppendStreamItemText(&builder, &previousItemID, text, s.itemID)
		display.Content = builder.String()
	} else {
		display.Content += text
	}
	s.itemContent += text
	if text != "" {
		display.Kind = core.MergeReasoningKind(display.Kind, event.ReasoningKind)
		// Any later delta invalidates a previous cumulative summary snapshot.
		display.SummaryParts = nil
	}
	if len(event.ReasoningSummaryParts) > 0 {
		display.Kind = core.MergeReasoningKind(display.Kind, core.ReasoningKindSummary)
		// Preserve provider structure only when it describes this entire burst,
		// not when the snapshot also contains earlier bursts or provider items.
		if s.burstPrefix == "" && display.Content == strings.Join(event.ReasoningSummaryParts, "\n\n") {
			display.SummaryParts = append([]string(nil), event.ReasoningSummaryParts...)
		}
	}
	display.Kind = core.NormalizeReasoningKind(display.Kind)
	display.SummaryTitle = ""
	if display.Kind == core.ReasoningKindSummary {
		display.SummaryTitle = internalreasoning.ParseReasoningSummary(display.Content).Title
	}
	part.ReasoningDisplay = &display
}

func (t *providerTurnFrame) appendInlineText(text string) {
	t.inlineReasoning.active = false
	if n := len(t.inlineSyncParts); n > 0 && t.inlineSyncParts[n-1].Type == core.PartText {
		t.inlineSyncParts[n-1].Text += text
		return
	}
	t.inlineSyncParts = append(t.inlineSyncParts, core.Part{Type: core.PartText, Text: text, CreatedAt: time.Now().UnixMilli()})
}

func (t *providerTurnFrame) observeInlineReasoning(event core.Event) {
	switch event.Type {
	case core.EventReasoningDelta:
		t.inlineReasoning.append(&t.inlineSyncParts, event)
	case core.EventToolCall, core.EventToolActivity, core.EventToolExecStart, core.EventToolExecEnd:
		// Provider activity retains its existing storage/projection behavior;
		// observing it here only closes the reasoning burst, not reorders it.
		t.inlineReasoning.active = false
	case core.EventAttemptDiscard:
		t.inlineSyncParts = nil
		t.inlineReasoning = inlineReasoningState{}
	}
}
