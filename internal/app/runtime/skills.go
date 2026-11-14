package runtime

import (
	"strings"

	"github.com/term-llm/term-llm/internal/config"
)

// ApplySkillsFlag applies the --skills flag value to modify skills config.
// Returns the modified config (or original if flag is empty).
//
// Flag values:
//   - "" (empty): use config defaults
//   - "none": disable skills entirely
//   - "all" or "*": enable all skills with auto mode
//   - "skill1,skill2": preload named instructions (disables model discovery)
//   - "skill1,+": preload named instructions + model discovery
func ApplySkillsFlag(cfg *config.SkillsConfig, flag string) *config.SkillsConfig {
	if flag == "" {
		return cfg
	}

	// Create a copy to avoid modifying the original
	result := *cfg

	switch strings.TrimSpace(flag) {
	case "none":
		result.Enabled = false
		return &result
	case "all", "*":
		// Enable all skills with auto mode
		result.Enabled = true
		result.AutoInvoke = true
		return &result
	}

	// Parse comma-separated skill names
	var skills []string
	hasPlus := false

	for _, part := range strings.Split(flag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "+" {
			hasPlus = true
		} else {
			skills = append(skills, part)
		}
	}

	// Enable skills with specified list
	if len(skills) > 0 {
		result.Enabled = true
		result.AlwaysEnabled = skills
		// When explicit skills are specified, auto-invoke is disabled
		// unless "+" is included to also include auto-discovered skills
		result.AutoInvoke = hasPlus
	}

	return &result
}
