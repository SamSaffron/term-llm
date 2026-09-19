package agents

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// instructionGatePattern matches a model gate marker occupying its own line,
// for example "[[[claude-bin:opus]]]" or "[[[all]]]". Leading indentation is
// capped at three spaces so a marker inside an indented code block stays
// literal text, matching CommonMark's rule for indented code.
var instructionGatePattern = regexp.MustCompile(`^ {0,3}\[\[\[([^\[\]]*)\]\]\] *$`)

// codeFencePattern matches a fenced code block delimiter so markers used as
// documentation examples inside the file are not treated as gates.
var codeFencePattern = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// FilterInstructionsForModel removes model-gated blocks that do not apply to
// the active provider/model.
//
// Gates are honoured only in the user-level ~/.config/term-llm/AGENTS.md.
// Project files are never passed through this filter, so a shared repository
// file is never rewritten based on the model and a marker written there stays
// inert literal text.
//
// A gate marker sits alone on a line and applies to every following line until
// the next gate marker or the end of the content:
//
//	[[[claude-bin:opus]]]
//	Avoid the word "load-bearing".
//	[[[all]]]
//	This applies everywhere again.
//
// A gate runs to the end of the file unless closed with [[[all]]], [[[*]]], or
// an empty [[[]]]. Markdown horizontal rules and headings do not close it, and
// markers inside fenced or indented code blocks are left alone.
//
// A gate lists one or more specs separated by "," or "|". Each spec is either
// "provider:model", "provider:*", ":model", or a bare token matched against
// both the provider key and the model name. Matching is case-insensitive.
// A spec matches a component when it is equal to it or is a prefix ending on a
// separator, so "opus" matches "opus-max" but not "opusculum", "opus2", or
// "claude-opus-5". Use "*" for broader matches: "*opus*" matches all three.
//
// Positive specs are OR-ed. A spec prefixed with "!" vetoes the block, so
// "[[[opus, !fable]]]" means "opus and not fable".
//
// When neither provider nor model is known, gate markers are stripped and all
// content is kept.
func FilterInstructionsForModel(content, provider, model string) string {
	if !strings.Contains(content, "[[[") {
		return content
	}

	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	unknownTarget := provider == "" && model == ""

	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	include := true
	sawGate := false
	fence := ""
	for _, line := range lines {
		if delim := openFenceDelimiter(line, fence); delim != "" || fence != "" {
			// Inside or toggling a fenced block: never interpret markers.
			fence = nextFenceState(line, fence, delim)
			if include {
				kept = append(kept, line)
			}
			continue
		}
		if match := instructionGatePattern.FindStringSubmatch(trimCR(line)); match != nil {
			sawGate = true
			include = unknownTarget || gateMatches(match[1], provider, model)
			continue
		}
		if include {
			kept = append(kept, line)
		}
	}

	if !sawGate {
		// Only an inline "[[[" mention; leave the content byte-identical.
		return content
	}
	return strings.Join(tidyBlankLines(kept), "\n")
}

// openFenceDelimiter returns the fence delimiter starting at line when no fence
// is currently open, or the current fence when one is.
func openFenceDelimiter(line, fence string) string {
	if fence != "" {
		return fence
	}
	if match := codeFencePattern.FindStringSubmatch(trimCR(line)); match != nil {
		return match[1]
	}
	return ""
}

// nextFenceState returns the fence state after processing line.
func nextFenceState(line, fence, delim string) string {
	if fence == "" {
		return delim
	}
	// A closing fence uses the same character and is at least as long.
	if match := codeFencePattern.FindStringSubmatch(trimCR(line)); match != nil {
		if match[1][0] == fence[0] && len(match[1]) >= len(fence) {
			return ""
		}
	}
	return fence
}

func trimCR(line string) string {
	return strings.TrimSuffix(line, "\r")
}

// tidyBlankLines collapses blank runs left behind by removed blocks and drops
// leading and trailing blank lines. It works per line so CRLF content keeps its
// line endings.
func tidyBlankLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blanks++
			continue
		}
		if blanks > 0 && len(out) > 0 {
			out = append(out, "")
		}
		blanks = 0
		out = append(out, line)
	}
	return out
}

// gateMatches reports whether a gate's spec list applies to provider/model.
// An empty gate is a reset and matches everything, like [[[all]]].
func gateMatches(specList, provider, model string) bool {
	var positives, matchedPositive bool
	for _, spec := range strings.FieldsFunc(specList, func(r rune) bool { return r == ',' || r == '|' }) {
		spec = strings.ToLower(strings.TrimSpace(spec))
		if spec == "" {
			continue
		}
		negated := false
		if strings.HasPrefix(spec, "!") {
			negated = true
			spec = strings.TrimSpace(strings.TrimPrefix(spec, "!"))
			if spec == "" {
				continue
			}
		}
		matched := spec == "all" || spec == "*" || specMatches(spec, provider, model)
		if negated {
			if matched {
				return false
			}
			continue
		}
		positives = true
		if matched {
			matchedPositive = true
		}
	}
	if !positives {
		// Empty gate or negations that did not veto.
		return true
	}
	return matchedPositive
}

// specMatches reports whether a single spec applies to provider/model.
func specMatches(spec, provider, model string) bool {
	if idx := strings.Index(spec, ":"); idx >= 0 {
		if componentMatches(provider, spec[:idx]) && componentMatches(model, spec[idx+1:]) {
			return true
		}
		// Model IDs may themselves contain a colon (for example the Ollama
		// "llama3.2:latest" form), so fall back to matching the whole spec.
		return componentMatches(model, spec)
	}
	return componentMatches(model, spec) || componentMatches(provider, spec)
}

// componentMatches compares one provider or model component against a spec
// fragment. An empty or "*" fragment matches anything. A fragment containing
// "*" is matched as a glob; otherwise it must equal the component or be a
// prefix of it ending on a separator.
func componentMatches(actual, want string) bool {
	if want == "" || want == "*" {
		return true
	}
	if actual == "" {
		return false
	}
	if actual == want {
		return true
	}
	if strings.Contains(want, "*") {
		return globMatches(actual, want)
	}
	if !strings.HasPrefix(actual, want) {
		return false
	}
	next, _ := utf8.DecodeRuneInString(actual[len(want):])
	return isSeparator(next)
}

// globMatches matches actual against a pattern whose only metacharacter is "*".
// Unlike path.Match, "*" also spans "/" so model IDs like
// "deepseek-ai/DeepSeek-V4-Flash" can be matched with a single wildcard.
func globMatches(actual, pattern string) bool {
	segments := strings.Split(pattern, "*")
	if len(segments) == 1 {
		return actual == pattern
	}
	if first := segments[0]; first != "" {
		if !strings.HasPrefix(actual, first) {
			return false
		}
		actual = actual[len(first):]
	}
	last := segments[len(segments)-1]
	middle := segments[1 : len(segments)-1]
	for _, segment := range middle {
		if segment == "" {
			continue
		}
		idx := strings.Index(actual, segment)
		if idx < 0 {
			return false
		}
		actual = actual[idx+len(segment):]
	}
	if last == "" {
		return true
	}
	return strings.HasSuffix(actual, last) && len(actual) >= len(last)
}

// isSeparator reports whether r ends a token in a provider key or model ID.
// Anything that is not a letter or digit separates, so "-", ".", "_", "/", and
// ":" all qualify.
func isSeparator(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}
