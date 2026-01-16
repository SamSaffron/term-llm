package prompt

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/samsaffron/term-llm/internal/input"
)

// ShouldUseUnifiedDiff determines if unified diff format should be used.
// diffFormat can be "auto", "udiff", or "replace".
func ShouldUseUnifiedDiff(model, diffFormat string) bool {
	switch diffFormat {
	case "udiff":
		return true
	case "replace":
		return false
	default: // "auto" or empty
		return false
	}
}

// StreamEditSystemPrompt builds a system prompt for streaming edit format.
// diffFormat: "auto", "udiff", or "replace"
func StreamEditSystemPrompt(instructions string, specs []EditSpec, model, diffFormat string) string {
	cwd, _ := os.Getwd()

	prompt := fmt.Sprintf(`You are an expert code editor. Make precise, minimal edits.

Context:
- OS: %s, Arch: %s
- CWD: %s`, runtime.GOOS, runtime.GOARCH, cwd)

	if instructions != "" {
		prompt += fmt.Sprintf("\n- Custom: %s", instructions)
	}

	// Use unified diff format based on model and config
	if ShouldUseUnifiedDiff(model, diffFormat) {
		prompt += `

## Output Format

Use unified diff format (standard patch format):

--- path/to/file.go
+++ path/to/file.go
@@ -10,6 +10,7 @@
 context line
-removed line
+added line
 context line

For multiple files, output multiple diff blocks:

--- first/file.go
+++ first/file.go
@@ ... @@
...

--- second/file.go
+++ second/file.go
@@ ... @@
...

## Rules

- The filename comes from the --- and +++ header lines
- Include @@ line number markers
- Lines starting with space are context (unchanged)
- Lines starting with - are removed
- Lines starting with + are added
- Include 2-3 lines of context around changes

## End with

[ABOUT]
Brief summary.
[/ABOUT]`
	} else {
		prompt += `

## Output Format

[FILE: path/to/file.go]
<<<<<<< SEARCH
exact copy from file
=======
replacement
>>>>>>> REPLACE
[/FILE]

## CRITICAL - Read Carefully

The SEARCH block must be COPIED EXACTLY from the file:
- Copy the exact text - do NOT retype or paraphrase
- Include exact whitespace, indentation, blank lines
- Do NOT add or remove blank lines
- Prefer single-line or few-line SEARCH blocks

WRONG (adds blank line that doesn't exist):
<<<<<<< SEARCH
// comment

func foo() {
=======

CORRECT (matches file exactly):
<<<<<<< SEARCH
// comment
func foo() {
=======

## Strategy

1. Make ONE small change per SEARCH/REPLACE block
2. For multiple typos, use multiple separate blocks
3. Keep SEARCH blocks to 1-3 lines max

WRONG (combining multiple changes):
<<<<<<< SEARCH
typo1
...
typo2
=======

CORRECT (separate blocks):
[FILE: example.go]
<<<<<<< SEARCH
typo1
=======
fixed1
>>>>>>> REPLACE
<<<<<<< SEARCH
typo2
=======
fixed2
>>>>>>> REPLACE
[/FILE]

## End with

[ABOUT]
Brief summary.
[/ABOUT]`
	}

	// Add guard info
	for _, spec := range specs {
		if spec.HasGuard {
			prompt += fmt.Sprintf("\n\n**IMPORTANT:** For %s, only modify lines %d-%d.",
				spec.Path, spec.StartLine, spec.EndLine)
		}
	}

	return prompt
}

// StreamEditUserPrompt builds the user prompt with file context.
// useUnifiedDiff determines which format instruction to include.
func StreamEditUserPrompt(request string, files []input.FileContent, specs []EditSpec, contextFiles []input.FileContent, stdinContent string, useUnifiedDiff bool) string {
	var sb strings.Builder

	sb.WriteString("Files to edit:\n\n")
	for _, f := range files {
		sb.WriteString(fmt.Sprintf("[FILE: %s]\n", f.Path))
		sb.WriteString(f.Content)
		if !strings.HasSuffix(f.Content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("[/FILE]\n\n")
	}

	// Add editable region blocks for guarded files
	for _, spec := range specs {
		if spec.HasGuard {
			for _, f := range files {
				if f.Path == spec.Path {
					excerpt := extractLineRange(f.Content, spec.StartLine, spec.EndLine)
					sb.WriteString(fmt.Sprintf("[EDITABLE-REGION: %s lines=%d-%d]\n",
						spec.Path, spec.StartLine, spec.EndLine))
					sb.WriteString(excerpt)
					if !strings.HasSuffix(excerpt, "\n") {
						sb.WriteString("\n")
					}
					sb.WriteString("[/EDITABLE-REGION]\n\n")
					break
				}
			}
		}
	}

	// Add read-only context files
	if len(contextFiles) > 0 {
		sb.WriteString("Reference files (read-only, do not edit):\n\n")
		for _, f := range contextFiles {
			sb.WriteString(fmt.Sprintf("[CONTEXT: %s]\n", f.Path))
			sb.WriteString(f.Content)
			if !strings.HasSuffix(f.Content, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("[/CONTEXT]\n\n")
		}
	}

	// Add stdin content as context (e.g., piped git diff)
	if stdinContent != "" {
		sb.WriteString("Piped input (read-only context):\n\n")
		sb.WriteString("[STDIN]\n")
		sb.WriteString(stdinContent)
		if !strings.HasSuffix(stdinContent, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("[/STDIN]\n\n")
	}

	sb.WriteString(fmt.Sprintf("Request: %s\n\n", request))

	if useUnifiedDiff {
		sb.WriteString("Output unified diff with --- and +++ headers, then [ABOUT]...[/ABOUT] summary.")
	} else {
		sb.WriteString("Respond with [FILE]...[/FILE] blocks for each change, then [ABOUT]...[/ABOUT] with a summary.")
	}

	return sb.String()
}

// LazyContextThreshold is the minimum file size (in lines) to use lazy context loading.
const LazyContextThreshold = 100

// LazyContextPadding is the number of lines to show around the editable region.
const LazyContextPadding = 10

// StreamEditSystemPromptLazy builds a system prompt for lazy context loading mode.
// This is similar to StreamEditSystemPrompt but adds info about the read_context tool.
func StreamEditSystemPromptLazy(instructions string, specs []EditSpec, model, diffFormat string) string {
	// Start with base prompt
	prompt := StreamEditSystemPrompt(instructions, specs, model, diffFormat)

	// Add read_context tool documentation
	prompt += `

## Context Tool

You have access to the read_context tool to fetch additional file context:
- read_context(path, start_line?, end_line?) - Read lines from a file

Only use this tool if you need context beyond what's shown. The editable region
and surrounding context are already provided. Most edits won't need extra context.`

	return prompt
}

// StreamEditUserPromptLazy builds a user prompt with lazy context loading.
// For large guarded files, only the editable region + padding is included.
// The LLM can use read_context tool to fetch more if needed.
func StreamEditUserPromptLazy(request string, files []input.FileContent, specs []EditSpec, contextFiles []input.FileContent, stdinContent string, useUnifiedDiff bool) string {
	var sb strings.Builder

	sb.WriteString("Files to edit:\n\n")

	for _, f := range files {
		lineCount := countLines(f.Content)

		// Find if this file has a guard
		var guard *EditSpec
		for i := range specs {
			if specs[i].Path == f.Path && specs[i].HasGuard {
				guard = &specs[i]
				break
			}
		}

		if guard != nil && lineCount > LazyContextThreshold {
			// Lazy mode: show only region + padding
			start := guard.StartLine - LazyContextPadding
			if start < 1 {
				start = 1
			}
			end := guard.EndLine + LazyContextPadding
			if end > lineCount {
				end = lineCount
			}

			sb.WriteString(fmt.Sprintf("[FILE: %s (%d lines total)]\n", f.Path, lineCount))
			sb.WriteString(fmt.Sprintf("[SHOWING: lines %d-%d]\n", start, end))
			excerpt := extractLineRange(f.Content, start, end)
			sb.WriteString(excerpt)
			if !strings.HasSuffix(excerpt, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("[/FILE]\n\n")

			sb.WriteString(fmt.Sprintf("Editable region: lines %d-%d\n", guard.StartLine, guard.EndLine))
			sb.WriteString("Use read_context tool if you need more context from this file.\n\n")
		} else {
			// Full file (small or no guard)
			sb.WriteString(fmt.Sprintf("[FILE: %s]\n", f.Path))
			sb.WriteString(f.Content)
			if !strings.HasSuffix(f.Content, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("[/FILE]\n\n")

			// Still show editable region marker for guarded files
			if guard != nil {
				sb.WriteString(fmt.Sprintf("Editable region: lines %d-%d\n\n", guard.StartLine, guard.EndLine))
			}
		}
	}

	// Add read-only context files
	if len(contextFiles) > 0 {
		sb.WriteString("Reference files (read-only, do not edit):\n\n")
		for _, f := range contextFiles {
			sb.WriteString(fmt.Sprintf("[CONTEXT: %s]\n", f.Path))
			sb.WriteString(f.Content)
			if !strings.HasSuffix(f.Content, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("[/CONTEXT]\n\n")
		}
	}

	// Add stdin content as context (e.g., piped git diff)
	if stdinContent != "" {
		sb.WriteString("Piped input (read-only context):\n\n")
		sb.WriteString("[STDIN]\n")
		sb.WriteString(stdinContent)
		if !strings.HasSuffix(stdinContent, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("[/STDIN]\n\n")
	}

	sb.WriteString(fmt.Sprintf("Request: %s\n\n", request))

	if useUnifiedDiff {
		sb.WriteString("Output unified diff with --- and +++ headers, then [ABOUT]...[/ABOUT] summary.")
	} else {
		sb.WriteString("Respond with [FILE]...[/FILE] blocks for each change, then [ABOUT]...[/ABOUT] with a summary.")
	}

	return sb.String()
}

// ShouldUseLazyContext determines if lazy context loading should be used.
// Returns true if any guarded file exceeds the threshold.
func ShouldUseLazyContext(files []input.FileContent, specs []EditSpec) bool {
	for _, spec := range specs {
		if !spec.HasGuard {
			continue
		}
		for _, f := range files {
			if f.Path == spec.Path {
				lineCount := countLines(f.Content)
				if lineCount > LazyContextThreshold {
					return true
				}
				break
			}
		}
	}
	return false
}

// countLines returns the number of lines in content.
func countLines(content string) int {
	if content == "" {
		return 0
	}
	count := strings.Count(content, "\n")
	// Add 1 if content doesn't end with newline (last line)
	if !strings.HasSuffix(content, "\n") {
		count++
	}
	return count
}
