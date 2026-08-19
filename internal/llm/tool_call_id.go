package llm

import "crypto/rand"

// newSyntheticToolCallID returns an opaque internal ID for a tool invocation
// whose provider protocol did not supply one. IDs must remain unique across
// turns, provider instances, engine runs, and persisted conversation history.
func newSyntheticToolCallID() string {
	return "call_" + rand.Text()
}
