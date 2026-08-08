package prompt

import "github.com/samsaffron/term-llm/internal/textmatch"

// EditSpec describes a file with optional line range guard.
type EditSpec struct {
	Path      string
	StartLine int // 1-indexed, 0 means from beginning
	EndLine   int // 1-indexed, 0 means to end
	HasGuard  bool
}

// extractLineRange extracts lines startLine to endLine (1-indexed, inclusive) from content.
func extractLineRange(content string, startLine, endLine int) string {
	return textmatch.NumberedLineRange(content, startLine, endLine)
}
