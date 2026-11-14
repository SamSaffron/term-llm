package prompt

import (
	"github.com/term-llm/term-llm/internal/input"
)

// AskSystemPrompt returns the system prompt for ask command.
// Returns empty string if no instructions configured (preserves current behavior).
func AskSystemPrompt(instructions string) string {
	return instructions
}

// AskUserPrompt formats a question with optional file context
func AskUserPrompt(question string, files []input.FileContent, stdin string) string {
	var result string

	// Add file context if provided
	fileContext := input.FormatFilesXML(files, stdin)
	if fileContext != "" {
		result = fileContext + "\n\n"
	}

	result += question
	return result
}
