package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// formatDefaultValue formats a default value for display.
func formatDefaultValue(val any) string {
	switch v := val.(type) {
	case string:
		if needsQuoting(v) {
			return fmt.Sprintf("%q", v)
		}
		return v
	case bool:
		return fmt.Sprintf("%t", v)
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	case []string:
		if len(v) == 0 {
			return "[]"
		}
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, quoteArrayItem(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case []int64:
		if len(v) == 0 {
			return "[]"
		}
		items := make([]string, 0, len(v))
		for _, item := range v {
			items = append(items, fmt.Sprintf("%d", item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case []config.LinkedDevicesICEServerConfig:
		encoded, err := json.Marshal(v)
		if err == nil {
			return string(encoded)
		}
	case map[string]any:
		if len(v) == 0 {
			return "{}"
		}
	}
	return fmt.Sprintf("%v", val)
}

func formatPlaceholderValue(val any) string {
	if val == nil {
		return "null"
	}
	return formatDefaultValue(val)
}
