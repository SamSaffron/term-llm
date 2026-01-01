package prompt

// AskSystemPrompt returns the system prompt for ask command.
// Returns empty string if no instructions configured (preserves current behavior).
func AskSystemPrompt(instructions string) string {
	return instructions
}
