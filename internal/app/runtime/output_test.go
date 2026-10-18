package runtime

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestRunnerOutputTaggedDiscardBeforeNewTurnOutput(t *testing.T) {
	t.Parallel()
	var output RunnerOutput
	output.Event(core.Event{Type: core.EventTextDelta, Text: "accepted", ProviderTurnIndexSet: true})
	// A restart discard can identify a new turn before it has emitted text.
	output.Event(core.Event{Type: core.EventAttemptDiscard, ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(core.Event{Type: core.EventTextDelta, Text: "abandoned", ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(core.Event{Type: core.EventAttemptDiscard, ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(core.Event{Type: core.EventTextDelta, Text: " replacement", ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	if got := output.Response(); got != "accepted replacement" {
		t.Fatalf("response = %q, want accepted replacement", got)
	}
}
