package llm

import "github.com/samsaffron/term-llm/internal/llm/core"

// newSyntheticToolCallID returns an opaque internal ID for a tool invocation
// whose provider protocol did not supply one.
func newSyntheticToolCallID() string { return core.NewSyntheticToolCallID() }
