package llm

import "strings"

// isLeadingReasoningWhitespaceArtifact reports whether a provider emitted a
// whitespace-only assistant text delta that should be treated as part of a
// hidden reasoning/thinking channel rather than visible assistant output.
//
// Some OpenAI-compatible reasoning models emit a pure-whitespace content prefix
// (commonly "\n\n") in the same chunk as reasoning/reasoning_content before any
// real assistant text. Rendering that prefix creates blank gaps before tool rows
// in ask. Keep this deliberately narrow: content-only leading whitespace is
// valid model output and must be preserved.
func isLeadingReasoningWhitespaceArtifact(content, reasoning string, sawVisibleText bool) bool {
	return !sawVisibleText && reasoning != "" && strings.TrimSpace(content) == ""
}

func hasVisibleTextDelta(text string) bool {
	return strings.TrimSpace(text) != ""
}
