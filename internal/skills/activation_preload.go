package skills

import (
	"fmt"
	"strings"
)

// Preload resolves an explicit instruction/capability selection, not a slash
// invocation. Discovery and user-invocable gates do not apply, argument
// placeholders stay literal, and context: fork does not launch an agent.
// Applying capabilities is left to the host, as with Activate.
func (a *Activator) Preload(name string) (*Activation, error) {
	name = strings.TrimSpace(name)
	if a == nil || a.registry == nil || name == "" {
		return nil, fmt.Errorf("skill not found: %q", name)
	}
	skill, err := a.registry.Get(name)
	if err != nil {
		return nil, err
	}
	metadata, err := InvocationFor(skill)
	if err != nil {
		return nil, err
	}
	defs := append([]SkillToolDef(nil), skill.Tools...)
	for i := range defs {
		defs[i].UserInstalled = skill.UserInstalled
	}
	return &Activation{
		Skill:               skill,
		Metadata:            metadata,
		Prompt:              skill.Body,
		BaseDir:             skill.SourcePath,
		Resources:           activationResources(skill),
		AllowedTools:        append([]string(nil), skill.AllowedTools...),
		AllowedToolsPresent: skill.AllowedToolsPresent,
		ToolDefs:            defs,
		Origin:              SkillActivationUser,
	}, nil
}
