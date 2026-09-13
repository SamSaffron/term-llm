package cmd

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
)

// runnerOutput retains earlier provider turns while allowing the current
// attempt's provisional output to be retracted. Usage is not a commit boundary:
// a provider can emit usage before its stream fails.
type runnerOutput struct {
	response          strings.Builder
	thinking          strings.Builder
	thinkingItemID    string
	turn              int
	responseStart     int
	thinkingStart     int
	thinkingItemStart string
}

func (o *runnerOutput) Event(ev llm.Event) {
	// Agentic model events carry a turn index; simple streams and engine-generated
	// retry discards may not. An untagged discard always refers to the current turn.
	if ev.ProviderTurnIndexSet && ev.ProviderTurnIndex > o.turn {
		o.turn = ev.ProviderTurnIndex
		o.responseStart = o.response.Len()
		o.thinkingStart = o.thinking.Len()
		o.thinkingItemStart = o.thinkingItemID
	}
	switch ev.Type {
	case llm.EventTextDelta:
		o.response.WriteString(ev.Text)
	case llm.EventReasoningDelta:
		internalreasoning.AppendStreamItemText(&o.thinking, &o.thinkingItemID, ev.Text, ev.ReasoningItemID)
	case llm.EventAttemptDiscard:
		restoreOutputPrefix(&o.response, o.responseStart)
		restoreOutputPrefix(&o.thinking, o.thinkingStart)
		o.thinkingItemID = o.thinkingItemStart
	}
}

func restoreOutputPrefix(builder *strings.Builder, length int) {
	prefix := builder.String()[:length]
	builder.Reset()
	builder.WriteString(prefix)
}
