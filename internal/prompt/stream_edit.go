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
		// Auto-detect: use unified diff for codex models
		return strings.Contains(strings.ToLower(model), "codex")
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
func StreamEditUserPrompt(request string, files []input.FileContent, specs []EditSpec, useUnifiedDiff bool) string {
	var sb strings.Builder

	sb.WriteString("Files to edit:\n\n")
	for _, f := range files {
		sb.WriteString(fmt.Sprintf("<file path=\"%s\">\n", f.Path))
		sb.WriteString(f.Content)
		if !strings.HasSuffix(f.Content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("</file>\n\n")
	}

	// Add editable region blocks for guarded files
	for _, spec := range specs {
		if spec.HasGuard {
			for _, f := range files {
				if f.Path == spec.Path {
					excerpt := extractLineRange(f.Content, spec.StartLine, spec.EndLine)
					sb.WriteString(fmt.Sprintf("<editable-region path=\"%s\" lines=\"%d-%d\">\n",
						spec.Path, spec.StartLine, spec.EndLine))
					sb.WriteString(excerpt)
					if !strings.HasSuffix(excerpt, "\n") {
						sb.WriteString("\n")
					}
					sb.WriteString("</editable-region>\n\n")
					break
				}
			}
		}
	}

	sb.WriteString(fmt.Sprintf("Request: %s\n\n", request))

	if useUnifiedDiff {
		sb.WriteString("Output unified diff with --- and +++ headers, then [ABOUT]...[/ABOUT] summary.")
	} else {
		sb.WriteString("Respond with [FILE]...[/FILE] blocks for each change, then [ABOUT]...[/ABOUT] with a summary.")
	}

	return sb.String()
}
