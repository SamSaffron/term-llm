package chat

import (
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/tools"
)

// applySkills refreshes admission-dependent prompt notes before the refreshed
// system message is persisted. A rejected module must not leave its old body in
// the prompt, and an admitted module must not retain a prompt-only warning.
func (c *RuntimeSystemContext) applySkills(eng *engine.Engine, manager *tools.ToolManager) {
	if c.ApplySkills != nil {
		c.ApplySkills(eng, manager)
	}
	c.SystemPrompt = c.Skills.RefreshPreloadedInstructions(c.SystemPrompt)
}
