package engine

import "github.com/samsaffron/term-llm/internal/llm/core"

// ContextContinuationPrompt is the control prompt appended after compaction.
// The engine appends it and compaction recognises and filters it from history.
const ContextContinuationPrompt = "Continue the task from the compacted context. Follow the pending next step; do not ask the user unless blocked."

// CompactionEnv injects the root-package behaviour compaction depends on.
// Every field is optional; a nil field falls back to the neutral behaviour
// noted on its accessor.
type CompactionEnv struct {
	// InputLimitForModel resolves a model's input token limit (nil = 0, unknown).
	InputLimitForModel func(model string) int
	// ClampOutputTokens caps a requested output budget for a model (nil = unchanged).
	ClampOutputTokens func(requested int, model string) int
	// IsolateProvider returns a provider safe for a one-shot helper request
	// (nil = the provider itself).
	IsolateProvider func(core.Provider) core.Provider
	// ForkProvider branches a provider's live conversation (nil = cannot fork).
	ForkProvider func(core.Provider) (core.Provider, bool)
	// ConversationStartFrom and InsertConversationStart carry the immutable
	// conversation-start anchor (nil = no anchor).
	ConversationStartFrom   func([]core.Message) (core.Message, bool)
	InsertConversationStart func(messages, source []core.Message) []core.Message
	// PlatformContextFrom and InsertPlatformContext carry platform context
	// (nil = no platform context).
	PlatformContextFrom   func([]core.Message) (core.Message, bool)
	InsertPlatformContext func(messages, source []core.Message) []core.Message
}

func (e CompactionEnv) inputLimitForModel(model string) int {
	if e.InputLimitForModel == nil {
		return 0
	}
	return e.InputLimitForModel(model)
}

func (e CompactionEnv) clampOutputTokens(requested int, model string) int {
	if e.ClampOutputTokens == nil {
		return requested
	}
	return e.ClampOutputTokens(requested, model)
}

func (e CompactionEnv) isolatedProvider(p core.Provider) core.Provider {
	if e.IsolateProvider == nil {
		return p
	}
	return e.IsolateProvider(p)
}

func (e CompactionEnv) forkProvider(p core.Provider) (core.Provider, bool) {
	if e.ForkProvider == nil {
		return nil, false
	}
	return e.ForkProvider(p)
}

func (e CompactionEnv) conversationStartFrom(messages []core.Message) (core.Message, bool) {
	if e.ConversationStartFrom == nil {
		return core.Message{}, false
	}
	return e.ConversationStartFrom(messages)
}

func (e CompactionEnv) insertConversationStart(messages, source []core.Message) []core.Message {
	if e.InsertConversationStart == nil {
		return messages
	}
	return e.InsertConversationStart(messages, source)
}

func (e CompactionEnv) platformContextFrom(messages []core.Message) (core.Message, bool) {
	if e.PlatformContextFrom == nil {
		return core.Message{}, false
	}
	return e.PlatformContextFrom(messages)
}

func (e CompactionEnv) insertPlatformContext(messages, source []core.Message) []core.Message {
	if e.InsertPlatformContext == nil {
		return messages
	}
	return e.InsertPlatformContext(messages, source)
}
