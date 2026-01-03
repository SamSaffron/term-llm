package prompt

import (
	"fmt"
	"os"
	"runtime"

	"github.com/samsaffron/term-llm/internal/input"
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
6. You MUST call the suggest_commands tool to provide your suggestions - do not respond with plain text
`, numSuggestions)

	if enableSearch {
		base += `7. You have access to web search. Use it to find current documentation, latest versions, or up-to-date syntax when relevant`
	}

	return base
}

// SuggestUserPrompt formats the user's request with optional file context
func SuggestUserPrompt(userInput string, files []input.FileContent, stdin string) string {
	var result string

	// Add file context if provided
	fileContext := input.FormatFilesXML(files, stdin)
	if fileContext != "" {
		result = fileContext + "\n\n"
	}

	result += fmt.Sprintf("I want to: %s", userInput)
	return result
}

// EditDescription is the description for the edit tool
const EditDescription = "Edit a file by replacing old_string with new_string. You may include the literal token <<<elided>>> in old_string to match any sequence of characters (including newlines). Use multiple tool calls for multiple edits."

// EditSchema returns the JSON schema for the edit tool
func EditSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file_path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file to edit",
			},
			"old_string": map[string]interface{}{
				"type":        "string",
				"description": "The exact text to find and replace. Include enough context to be unique. You may include the literal token <<<elided>>> to match any sequence of characters (including newlines).",
			},
			"new_string": map[string]interface{}{
				"type":        "string",
				"description": "The text to replace old_string with",
			},
		},
		"required":             []string{"file_path", "old_string", "new_string"},
		"additionalProperties": false,
	}
}

// UnifiedDiffDescription is the description for the unified diff tool
const UnifiedDiffDescription = `Apply file edits using unified diff format. Output a single diff containing all changes.

Format:
--- path/to/file
+++ path/to/file
@@ context to locate (e.g., func Name) @@
 context line (unchanged, space prefix)
-line to remove
+line to add

Elision (-...) for replacing large blocks:
-func Example() {
-...
-}
+func Example() { return nil }

The -... matches everything between the start anchor (-func Example...) and end anchor (-}).
IMPORTANT: After -... you MUST include an end anchor (another - line) so we know where elision stops.

Rules:
1. @@ headers help locate changes - use function/class names, not line numbers
2. Context lines (space prefix) anchor the position - must match file exactly
3. Use -... ONLY when replacing 10+ lines; for small changes list all - lines explicitly
4. After -... always include the closing line (e.g., -}) as the end anchor
5. Multiple files: use separate --- +++ blocks for each file`

// UnifiedDiffSchema returns the JSON schema for the unified diff tool
func UnifiedDiffSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"diff": map[string]interface{}{
				"type":        "string",
				"description": "Unified diff with all changes. Format: --- and +++ for paths, @@ for context headers, space prefix for context lines, - for removals, + for additions. Use -... to elide large removed blocks (must have end anchor after).",
			},
		},
		"required":             []string{"diff"},
		"additionalProperties": false,
	}
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
