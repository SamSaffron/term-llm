package prompt

import (
	"fmt"
	"os"
	"runtime"
)

// SuggestSystemPrompt returns the system prompt for command suggestions
func SuggestSystemPrompt(shell, instructions string, numSuggestions int, enableSearch bool) string {
	cwd, _ := os.Getwd()
	base := fmt.Sprintf(`You are a CLI command expert. The user will describe what they want to do, and you must suggest exactly %d shell commands that accomplish their goal.

Context:
- Operating System: %s
- Architecture: %s
- Shell: %s
- Current Directory: %s`, numSuggestions, runtime.GOOS, runtime.GOARCH, shell, cwd)

	if instructions != "" {
		base += fmt.Sprintf(`
- User Context: %s`, instructions)
	}

	base += fmt.Sprintf(`

Rules:
1. Suggest exactly %d different commands or approaches
2. Each command should be a valid, executable shell command
3. Prefer common tools that are likely to be installed
4. Order suggestions from most likely to be useful to least
5. Keep explanations brief (under 50 words)
6. If the request is dangerous (rm -rf /, etc), still provide the command but warn in the explanation`, numSuggestions)

	if enableSearch {
		base += `
7. You have access to web search. Use it to find current documentation, latest versions, or up-to-date syntax when relevant
8. After searching (if needed), call the suggest_commands tool with your suggestions`
	}

	return base
}

// SuggestUserPrompt formats the user's request
func SuggestUserPrompt(userInput string) string {
	return fmt.Sprintf("I want to: %s", userInput)
}

// SuggestSchema returns the JSON schema for structured output
func SuggestSchema(numSuggestions int) map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"suggestions": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]interface{}{
						"command": map[string]interface{}{
							"type":        "string",
							"description": "The shell command to execute",
						},
						"explanation": map[string]interface{}{
							"type":        "string",
							"description": "Brief explanation of what the command does",
						},
						"likelihood": map[string]interface{}{
							"type":        "integer",
							"description": "How likely this command matches user intent (1=unlikely, 10=very likely)",
						},
					},
					"required": []string{"command", "explanation", "likelihood"},
				},
				"minItems": numSuggestions,
				"maxItems": numSuggestions,
			},
		},
		"required": []string{"suggestions"},
	}
}
