package cmd

import (
	"github.com/spf13/cobra"
	"github.com/term-llm/term-llm/internal/config"
)

// Capture explicitness as well as value: --allow-project-skill-tools=false must
// override user config, and later flag changes must not affect rebuilt sessions.
func projectSkillToolsOverride(cmd *cobra.Command) *bool {
	if cmd == nil || !cmd.Flags().Changed("allow-project-skill-tools") {
		return nil
	}
	value, _ := cmd.Flags().GetBool("allow-project-skill-tools")
	return &value
}

func projectSkillToolsConfigLoader(cmd *cobra.Command, load func() (*config.Config, error)) func() (*config.Config, error) {
	override := projectSkillToolsOverride(cmd)
	return func() (*config.Config, error) {
		cfg, err := load()
		if err != nil {
			return nil, err
		}
		if override != nil {
			cfg.Skills.AllowProjectScriptTools = *override
		}
		return cfg, nil
	}
}
