package testutil

import (
	"regexp"
	"strings"
	"testing"
)

// AssertContains fails the test if output does not contain expected.
func AssertContains(t *testing.T, output, expected string) {
	t.Helper()
	if !strings.Contains(output, expected) {
		t.Errorf("output does not contain expected string\nExpected to find: %q\nIn output:\n%s", expected, truncateForError(output))
	}
}

// AssertContainsPlain fails if output (after stripping ANSI) does not contain expected.
func AssertContainsPlain(t *testing.T, output, expected string) {
	t.Helper()
	plain := StripANSI(output)
	if !strings.Contains(plain, expected) {
		t.Errorf("output does not contain expected string\nExpected to find: %q\nIn output (plain):\n%s", expected, truncateForError(plain))
	}
}

// AssertNotContains fails the test if output contains unexpected.
func AssertNotContains(t *testing.T, output, unexpected string) {
	t.Helper()
	if strings.Contains(output, unexpected) {
		t.Errorf("output contains unexpected string\nDid not expect to find: %q\nIn output:\n%s", unexpected, truncateForError(output))
	}
}

// AssertNotContainsPlain fails if output (after stripping ANSI) contains unexpected.
func AssertNotContainsPlain(t *testing.T, output, unexpected string) {
	t.Helper()
	plain := StripANSI(output)
	if strings.Contains(plain, unexpected) {
		t.Errorf("output contains unexpected string\nDid not expect to find: %q\nIn output (plain):\n%s", unexpected, truncateForError(plain))
	}
}

// AssertMatches fails the test if output does not match the regex pattern.
func AssertMatches(t *testing.T, output string, pattern *regexp.Regexp) {
	t.Helper()
	if !pattern.MatchString(output) {
		t.Errorf("output does not match pattern\nPattern: %s\nOutput:\n%s", pattern.String(), truncateForError(output))
	}
}

// AssertMatchesPlain fails if output (after stripping ANSI) does not match pattern.
func AssertMatchesPlain(t *testing.T, output string, pattern *regexp.Regexp) {
	t.Helper()
	plain := StripANSI(output)
	if !pattern.MatchString(plain) {
		t.Errorf("output does not match pattern\nPattern: %s\nOutput (plain):\n%s", pattern.String(), truncateForError(plain))
	}
}

// AssertMatchesString fails the test if output does not match the regex pattern string.
func AssertMatchesString(t *testing.T, output, pattern string) {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("invalid regex pattern %q: %v", pattern, err)
	}
	AssertMatches(t, output, re)
}

// AssertToolPhaseShown verifies that a tool execution phase was displayed.
// It checks for common patterns like "Reading main.go" or "Searching: query".
func AssertToolPhaseShown(t *testing.T, output, toolName, info string) {
	t.Helper()
	plain := StripANSI(output)

	// Build patterns based on common tool phase formats
	var patterns []string
	switch toolName {
	case "web_search":
		patterns = []string{
			"Searching: " + info,
			"Searched: " + info,
		}
	case "read_url":
		patterns = []string{
			"Reading " + info,
			"Read URL: " + info,
		}
	case "read_file":
		patterns = []string{
			"Reading " + info,
			"Read " + info,
		}
	case "shell":
		patterns = []string{
			"Running " + info,
			"Ran " + info,
		}
	case "grep":
		patterns = []string{
			"Searching /" + info,
			"Searched /" + info,
		}
	case "glob":
		patterns = []string{
			"Finding " + info,
			"Found " + info,
		}
	default:
		// Generic check for tool name
		patterns = []string{toolName, info}
	}

	for _, p := range patterns {
		if strings.Contains(plain, p) {
			return // Found a match
		}
	}

	t.Errorf("tool phase not shown\nTool: %s\nInfo: %s\nExpected one of: %v\nIn output:\n%s",
		toolName, info, patterns, truncateForError(plain))
}

// AssertApprovalShown verifies that an approval prompt was displayed.
func AssertApprovalShown(t *testing.T, output, description string) {
	t.Helper()
	plain := StripANSI(output)

	// Check for common approval patterns
	patterns := []string{
		description,
		"Allow",
		"Approve",
		"[y/n]",
		"Yes",
		"No",
	}

	foundDesc := strings.Contains(plain, description)
	foundPrompt := false
	for _, p := range patterns[1:] {
		if strings.Contains(plain, p) {
			foundPrompt = true
			break
		}
	}

	if !foundDesc && !foundPrompt {
		t.Errorf("approval prompt not shown\nExpected description: %q\nIn output:\n%s",
			description, truncateForError(plain))
	}
}

// AssertLineCount fails if the output doesn't have the expected number of lines.
func AssertLineCount(t *testing.T, output string, expected int) {
	t.Helper()
	lines := strings.Split(output, "\n")
	if len(lines) != expected {
		t.Errorf("expected %d lines, got %d\nOutput:\n%s", expected, len(lines), truncateForError(output))
	}
}

// truncateForError truncates output for error messages to avoid huge logs.
func truncateForError(s string) string {
	const maxLen = 2000
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... [truncated]"
}
