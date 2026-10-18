package core

import "crypto/rand"

// NewSyntheticToolCallID returns an opaque internal ID for a tool invocation
// whose provider protocol did not supply one. IDs must remain unique across
// turns, provider instances, engine runs, and persisted conversation history.
func NewSyntheticToolCallID() string {
	return "call_" + rand.Text()
}
