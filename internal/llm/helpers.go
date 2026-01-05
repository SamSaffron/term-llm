package llm

import "strings"

func chooseModel(requested, fallback string) string {
	if strings.TrimSpace(requested) != "" {
		return requested
	}
	return fallback
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
