package tool

import (
	"fmt"
	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/skills"
)

// AdmitSkill snapshots a declared module without executing it or granting tools.
func AdmitSkill(eng *engine.Engine, activation *skills.Activation, trusted func() bool) error {
	if activation == nil || activation.Skill == nil || activation.Skill.CodeMode == nil {
		return nil
	}
	skill := activation.Skill
	if eng == nil || trusted == nil {
		return fmt.Errorf("skill %s code modules require an engine and local tool manager", skill.Name)
	}
	if !codeModeEnabled(eng) {
		return fmt.Errorf("skill %s requires enabled permitted spawn_script (or legacy code_mode)", skill.Name)
	}
	if !trusted() {
		return fmt.Errorf("skill %s: project/unclassified code modules require skills.allow_project_code_modules or current yolo", skill.Name)
	}
	m := skill.CodeMode
	snapshot, err := codemode.Snapshot(skill.Name, activation.BaseDir, codemode.SourceManifest{Module: m.Module, Files: m.Files, Types: m.Types})
	if err != nil {
		return fmt.Errorf("snapshot skill %s modules: %w", skill.Name, err)
	}
	if err = eng.AdmitCodeModule(skill.Name, snapshot, m.Requires.Tools, trusted); err != nil {
		return err
	}
	return nil
}

// codeModeEnabled accepts the session-managed spawn_script tool or the explicit legacy
// synchronous compatibility tool. Registering one is enough to admit modules;
// the module sees whatever cell authority the chosen path provides.
func codeModeEnabled(eng *engine.Engine) bool {
	if eng == nil {
		return false
	}
	for _, name := range []string{ExecName, Name} {
		if _, ok := eng.Tools().Get(name); ok && eng.IsToolAllowed(name) {
			return true
		}
	}
	return false
}

// OriginTrust uses host-discovered installation scope, never frontmatter claims.
func OriginTrust(eng *engine.Engine, a *skills.Activation, cfg *config.Config, yolo interface{ YoloEnabled() bool }) func() bool {
	if a == nil || a.Skill == nil {
		return nil
	}
	installed := a.Skill.UserInstalled || a.Skill.Source == skills.SourceBuiltin
	return func() bool {
		return codeModeEnabled(eng) && (installed || (cfg != nil && cfg.Skills.AllowProjectCodeModules) || (yolo != nil && yolo.YoloEnabled()))
	}
}

// ActivationInstructions describes admission without running module top-level code.
func ActivationInstructions(a *skills.Activation, prompt string) string {
	response := skills.GenerateActivationResponse(a.Skill, prompt)
	if module := a.Skill.CodeMode; module != nil {
		response += fmt.Sprintf("\n\nCode mode module admitted: import('skill:%s'). Dependencies (not grants): %v. All code shares the cell's admitted authority. Express it with spawn_script when available: load()/store() state is committed automatically after successful cells, and wait_script/cancel_script manage the cell lifecycle. The legacy code_mode compatibility path instead requires passing each successful call's returned state explicitly. Screenshots must be explicitly emitted with image(...).", a.Skill.Name, module.Requires.Tools)
	}
	return response
}

// ActivationAdmission is the error-propagating executable-module admission hook.
type ActivationAdmission struct {
	admit func(*skills.Activation) error
}

func (a *ActivationAdmission) SetOnCodeModuleActivated(cb func(*skills.Activation) error) {
	a.admit = cb
}
func (a *ActivationAdmission) AdmitCodeModule(activation *skills.Activation) error {
	if activation == nil || activation.Skill == nil || activation.Skill.CodeMode == nil {
		return nil
	}
	if a.admit == nil {
		return fmt.Errorf("code module admission unavailable")
	}
	return a.admit(activation)
}
