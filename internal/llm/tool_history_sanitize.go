package llm

import "github.com/samsaffron/term-llm/internal/llm/core"

// FilterConversationMessages removes durable UI/session event markers and other
// non-conversation rows before context is sent to providers, compaction, handover,
// or token estimation. It returns a shallow copy preserving cache anchors.
func FilterConversationMessages(messages []core.Message) []core.Message {
	return core.FilterConversationMessages(messages)
}
