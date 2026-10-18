package llm

import "github.com/samsaffron/term-llm/internal/llm/core"

// TextStreamResult is the common output of one-shot helper conversations such
// as side questions, compaction summaries, and handover briefs.
// Deprecated: use core.TextStreamResult in new packages.
type TextStreamResult = core.TextStreamResult

// CollectTextStream drains a provider stream while consistently handling text,
// displayable reasoning summaries, usage, retries, and provider error events.
// See core.CollectTextStream.
// Deprecated: use core.CollectTextStream in new packages.
func CollectTextStream(stream Stream, observer func(Event) error) (TextStreamResult, error) {
	return core.CollectTextStream(stream, observer)
}
