package session

import "github.com/samsaffron/term-llm/internal/llm"

// ProjectSelectedSessionPrompt applies an owning surface's process-local
// selection to the leading application system row only. It never searches past
// human/developer context or removes arbitrary later client system messages.
// The caller must opt in after successfully preparing session inputs.
func ProjectSelectedSessionPrompt(messages []llm.Message, prompt string) []llm.Message {
	if len(messages) == 0 || messages[0].Role != llm.RoleSystem {
		return messages
	}
	out := append([]llm.Message(nil), messages...)
	if prompt == "" {
		return out[1:]
	}
	out[0].Parts = llm.SystemText(prompt).Parts
	return out
}
