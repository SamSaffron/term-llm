package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// WarnUnknownParams checks args JSON for keys not in knownKeys.
// Returns a warning string (with trailing newline) to prepend to tool output,
// or "" if no unknown keys found.
func WarnUnknownParams(args json.RawMessage, knownKeys []string) string {
	var m map[string]interface{}
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	known := make(map[string]bool, len(knownKeys))
	for _, k := range knownKeys {
		known[k] = true
	}
	var unknown []string
	for k := range m {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	sort.Strings(unknown)
	var sb strings.Builder
	for _, k := range unknown {
		sb.WriteString(fmt.Sprintf("Unknown parameter '%s' was ignored\n", k))
	}
	return sb.String()
}
