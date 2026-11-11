package config

import "fmt"

// MemoryConfig adds declarative detectors to always-on native memory protection.
// Patterns are compiled and validated before memory provider calls or mutations.
// There is deliberately no executable, environment hook or enable/disable flag.
type MemoryConfig struct {
	SecretPatterns []string `mapstructure:"secret_patterns" yaml:"secret_patterns,omitempty"`
}

// validateMemorySettings runs before the general weak decoder: security data
// must not silently accept a command/disable field, scalar list, or non-string
// rule. Never include an unknown key or value in the diagnostic.
func validateMemorySettings(settings map[string]any) error {
	raw, ok := settings["memory"]
	if !ok || raw == nil {
		return nil
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("memory must be a declarative policy mapping")
	}
	for key, value := range block {
		if key != "secret_patterns" {
			return fmt.Errorf("unknown memory policy option")
		}
		switch values := value.(type) {
		case nil, []string:
		case []any:
			for _, v := range values {
				if _, ok := v.(string); !ok {
					return fmt.Errorf("memory.secret_patterns must be a list of strings")
				}
			}
		default:
			return fmt.Errorf("memory.secret_patterns must be a list of strings")
		}
	}
	return nil
}
