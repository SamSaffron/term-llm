package cmd

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

func appendChildSkillSystemContext(systemPrompt string, skill *runpkg.SkillRunMetadata) string {
	if skill == nil {
		return systemPrompt
	}
	var context strings.Builder
	context.WriteString("<isolated_skill_run>\n")
	context.WriteString("This is a direct user-invoked isolated skill run. Follow the supplied task without requesting parent conversation context.\n")
	context.WriteString("Skill: " + skill.Name + "\n")
	context.WriteString("Source: " + skill.Source + "\n")
	context.WriteString("Base directory: " + skill.SourcePath + "\n")
	if len(skill.Resources) > 0 {
		context.WriteString("Resources:\n")
		for _, resource := range skill.Resources {
			context.WriteString("- " + resource + "\n")
		}
	}
	context.WriteString("</isolated_skill_run>")
	if strings.TrimSpace(systemPrompt) == "" {
		return context.String()
	}
	return systemPrompt + "\n\n" + context.String()
}

func applyChildSkillRuntime(engine *engine.Engine, toolMgr *tools.ToolManager, skill *runpkg.SkillRunMetadata) error {
	if skill == nil {
		return nil
	}
	if len(skill.ToolDefs) > 0 {
		if toolMgr == nil {
			return fmt.Errorf("isolated skill %q declares tools, but child agent has no local tool registry", skill.Name)
		}
		if err := toolMgr.Registry.RegisterSkillTools(skill.ToolDefs, skill.SourcePath); err != nil {
			return fmt.Errorf("register isolated skill %q tools: %w", skill.Name, err)
		}
		for _, definition := range skill.ToolDefs {
			if tool, ok := toolMgr.Registry.Get(definition.Name); ok {
				engine.AddDynamicTool(tool)
			}
		}
	}
	if skill.AllowedToolsPresent {
		engine.SetAllowedToolsFilter(skill.AllowedTools)
	}
	return nil
}

func filterEngineAllowedToolSpecs(specs []core.ToolSpec, engine *engine.Engine) []core.ToolSpec {
	if engine == nil {
		return specs
	}
	return engine.FilterAllowedToolSpecs(specs)
}
