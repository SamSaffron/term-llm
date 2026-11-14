package tools

import (
	"errors"
	"fmt"

	codemodetool "github.com/term-llm/term-llm/internal/codemode/tool"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/skills"
)

// RegisterSkillScriptTools exposes normally scoped tools without executing or granting them.
func RegisterSkillScriptTools(eng *engine.Engine, manager *ToolManager, defs []skills.SkillToolDef, skillDir string) error {
	if len(defs) == 0 {
		return nil
	}
	if eng == nil || manager == nil || manager.Registry == nil {
		return fmt.Errorf("skill script tools require an engine and local tool manager")
	}
	if err := manager.Registry.RegisterSkillTools(defs, skillDir); err != nil {
		return fmt.Errorf("register script tools: %w", err)
	}
	for _, def := range defs {
		if tool, ok := manager.Registry.Get(def.Name); ok {
			if err := eng.AddDynamicTool(tool); err != nil {
				return fmt.Errorf("register skill tool %q: %w", def.Name, err)
			}
		}
	}
	return nil
}

// PreloadSkillCapabilities reapplies capabilities after runtime tool registration.
// It never executes or grants tools. Refresh earlier text with setup.RefreshPreloadedInstructions.
func PreloadSkillCapabilities(eng *engine.Engine, manager *ToolManager, setup *skills.Setup) error {
	if eng == nil {
		_, err := setup.PreloadedInstructions()
		return err
	}
	_, err := setup.Preload(func(activation *skills.Activation) (string, error) {
		missingScripts := len(activation.ToolDefs) > 0 && (manager == nil || manager.Registry == nil)
		if !missingScripts {
			if err := RegisterSkillScriptTools(eng, manager, activation.ToolDefs, activation.BaseDir); err != nil {
				return "", err
			}
		}
		admissionErr := AdmitSkillCodeModule(eng, manager, activation)
		// Probe availability with no trust; never turn a callable module into a grant.
		if (manager == nil || manager.Registry == nil) && errors.Is(codemodetool.AdmitSkill(eng, activation, func() bool { return false }), codemodetool.ErrUnavailable) {
			admissionErr = codemodetool.ErrUnavailable
		}
		admitted, err := codemodetool.Admitted(admissionErr)
		if err != nil {
			return "", err
		}
		response := codemodetool.ActivationInstructions(activation, "", admitted)
		if missingScripts {
			response += "\n\nDeclared script tools are not available here because there is no local tool manager. Follow the instructions above without them."
		}
		return response, nil
	})
	return err
}
