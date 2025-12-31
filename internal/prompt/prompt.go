package prompt

import (
	"fmt"
	"os"
	"runtime"
)

// SystemPrompt returns the system prompt for command suggestions
func SystemPrompt(shell string, customContext string) string {
	cwd, _ := os.Getwd()
	base := fmt.Sprintf(`You are a CLI command expert. The user will describe what they want to do, and you must suggest exactly 3 shell commands that accomplish their goal.

Context:
- Operating System: %s
- Architecture: %s
- Shell: %s
- Current Directory: %s`, runtime.GOOS, runtime.GOARCH, shell, cwd)

	if customContext != "" {
		base += fmt.Sprintf(`
- User Context: %s`, customContext)
	}

	base += `

Rules:
1. Suggest exactly 3 different commands or approaches
2. Each command should be a valid, executable shell command
3. Prefer common tools that are likely to be installed
4. Order suggestions from most likely to be useful to least
5. Keep explanations brief (under 50 words)
6. If the request is dangerous (rm -rf /, etc), still provide the command but warn in the explanation`

	return base
}

// UserPrompt formats the user's request
func UserPrompt(userInput string) string {
	return fmt.Sprintf("I want to: %s", userInput)
}

// JSONSchema returns the JSON schema for structured output
func JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"suggestions": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
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
							"minimum":     1,
							"maximum":     10,
							"description": "How likely this command matches user intent (1=unlikely, 10=very likely)",
						},
					},
					"required": []string{"command", "explanation", "likelihood"},
				},
				"minItems": 3,
				"maxItems": 3,
			},
		},
		"required": []string{"suggestions"},
	}
}
