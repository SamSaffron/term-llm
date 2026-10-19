package core

import "strings"

// ChooseModel returns requested unless it is blank, in which case fallback is
// returned.
func ChooseModel(requested, fallback string) string {
	if strings.TrimSpace(requested) != "" {
		return requested
	}
	return fallback
}

// Truncate shortens s to at most maxLen bytes, appending "..." when cut.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
