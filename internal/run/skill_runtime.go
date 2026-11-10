package run

import (
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

// ApplySkillRuntime installs a host-resolved isolated child's tools, temporary
// policy and immutable module admission. Dependencies never grant tools.
func ApplySkillRuntime(eng *engine.Engine, manager *tools.ToolManager, skill *SkillRunMetadata) error {
	if skill == nil {
		return nil
	}
	if len(skill.ToolDefs) > 0 {
		if manager == nil {
			return fmt.Errorf("isolated skill %q declares tools, but child agent has no local tool registry", skill.Name)
		}
		if err := manager.Registry.RegisterSkillTools(skill.ToolDefs, skill.SourcePath); err != nil {
			return fmt.Errorf("register isolated skill %q tools: %w", skill.Name, err)
		}
		for _, definition := range skill.ToolDefs {
			if tool, ok := manager.Registry.Get(definition.Name); ok {
				eng.AddDynamicTool(tool)
			}
		}
	}
	if skill.AllowedToolsPresent {
		eng.SetAllowedToolsFilter(skill.AllowedTools)
	}
	if skill.CodeModeSkill != nil {
		return tools.AdmitSkillCodeModule(eng, manager, &skills.Activation{Skill: skill.CodeModeSkill, BaseDir: skill.SourcePath})
	}
	return nil
}
