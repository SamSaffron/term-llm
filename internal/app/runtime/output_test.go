package runtime

import (
	"github.com/samsaffron/term-llm/internal/llm"
	"testing"
)

func TestRunnerOutputTaggedDiscardBeforeNewTurnOutput(t *testing.T) {
	t.Parallel()
	var output RunnerOutput
	output.Event(llm.Event{Type: llm.EventTextDelta, Text: "accepted", ProviderTurnIndexSet: true})
	// A restart discard can identify a new turn before it has emitted text.
	output.Event(llm.Event{Type: llm.EventAttemptDiscard, ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(llm.Event{Type: llm.EventTextDelta, Text: "abandoned", ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(llm.Event{Type: llm.EventAttemptDiscard, ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(llm.Event{Type: llm.EventTextDelta, Text: " replacement", ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	if got := output.Response(); got != "accepted replacement" {
		t.Fatalf("response = %q, want accepted replacement", got)
	}
}
