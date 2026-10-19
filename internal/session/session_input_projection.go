package session

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// ProjectSelectedSessionPrompt applies an owning surface's process-local
// selection to the leading application system row only. It never searches past
// human/developer context or removes arbitrary later client system messages.
// The caller must opt in after successfully preparing session inputs.
func ProjectSelectedSessionPrompt(messages []core.Message, prompt string) []core.Message {
	if len(messages) == 0 || messages[0].Role != core.RoleSystem {
		return messages
	}
	out := append([]core.Message(nil), messages...)
	if prompt == "" {
		return out[1:]
	}
	out[0].Parts = core.SystemText(prompt).Parts
	return out
}
