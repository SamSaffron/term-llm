package chat

import "strings"

func guardianFooterTone(message string) string {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.HasPrefix(lower, "guardian: denied"), strings.Contains(lower, "failed"), strings.Contains(lower, "unavailable"), strings.Contains(lower, "circuit breaker"):
		return "warning"
	case strings.HasPrefix(lower, "guardian: approved"):
		return "success"
	default:
		return "muted"
	}
}
