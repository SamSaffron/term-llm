package config

import "fmt"

// MemoryConfig is trusted owner-authored data, never derived from mined text.
// Empty lists select native defaults; disabling all permits explicit full optout.
type MemoryConfig struct {
	Secrets      MemorySecretsConfig `mapstructure:"secrets" yaml:"secrets,omitempty"`
	ExcludePaths []string            `mapstructure:"exclude_paths" yaml:"exclude_paths,omitempty"`
}
type MemorySecretsConfig struct {
	Disable  []string `mapstructure:"disable" yaml:"disable,omitempty"`
	Patterns []string `mapstructure:"patterns" yaml:"patterns,omitempty"`
}

func stringList(value any, key string) error {
	switch list := value.(type) {
	case nil, []string:
		return nil
	case []any:
		for _, v := range list {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s must be a list of strings", key)
			}
		}
		return nil
	}
	return fmt.Errorf("%s must be a list of strings", key)
}

// Validate strictly before the general weak decoder. Pattern literals are never
// echoed. Unknown option names are reported so owner typos are actionable.
func validateMemorySettings(settings map[string]any) error {
	raw := settings["memory"]
	if raw == nil {
		return nil
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("memory must be a declarative policy mapping")
	}
	for key, value := range block {
		switch key {
		case "exclude_paths":
			if err := stringList(value, "memory.exclude_paths"); err != nil {
				return err
			}
		case "secrets":
			if value == nil {
				continue
			}
			secrets, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("memory.secrets must be a mapping")
			}
			for name, list := range secrets {
				if name != "disable" && name != "patterns" {
					return fmt.Errorf("unknown memory.secrets option %q", name)
				}
				if err := stringList(list, "memory.secrets."+name); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown memory policy option %q", key)
		}
	}
	return nil
}
