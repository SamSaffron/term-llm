package ui

import "strings"

// ParseBoolDefault parses a boolean-like environment value with a fallback default.
// True values: 1, true, yes, on, y
// False values: 0, false, no, off, n
// Empty/unknown values return defaultValue.
func ParseBoolDefault(raw string, defaultValue bool) bool {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return defaultValue
	}
	switch value {
	case "1", "true", "yes", "on", "y":
		return true
	case "0", "false", "no", "off", "n":
		return false
	default:
		return defaultValue
	}
}
