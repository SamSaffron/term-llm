package chat

import (
	"os"
	"strings"
)

// TerminalTitleEnvironment captures environment variables used by terminal title
// providers. It is intentionally generic so provider-specific environment
// details stay inside the provider implementation.
type TerminalTitleEnvironment struct {
	values map[string]string
}

// NewTerminalTitleEnvironment builds a deterministic environment snapshot for
// tests or callers that already captured environment variables.
func NewTerminalTitleEnvironment(values map[string]string) TerminalTitleEnvironment {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		clone[key] = value
	}
	return TerminalTitleEnvironment{values: clone}
}

// TerminalTitleEnvironmentFromEnv snapshots the current process environment for
// terminal title providers.
func TerminalTitleEnvironmentFromEnv() TerminalTitleEnvironment {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		values[key] = value
	}
	return TerminalTitleEnvironment{values: values}
}

func (e TerminalTitleEnvironment) Get(name string) string {
	if e.values == nil {
		return ""
	}
	return e.values[name]
}
