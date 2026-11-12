package skills

import (
	"errors"
	"fmt"
	"strings"
)

// PreloadAdmission registers a resolved skill's capabilities and returns its
// admission-aware instructions. It must not execute scripts/modules, fork an
// agent, or apply allowed-tools to the parent session. An error rejects that
// skill's instructions without dropping the other selections.
type PreloadAdmission func(*Activation) (string, error)

// Preload resolves explicit per-run selections once. A non-nil callback admits
// capabilities on every call, so hosts can restore a prior skill context or
// register the same selection on another engine. Admission replaces prior
// admission errors and instructions. A nil callback only reads the latest result,
// initially instructions with unavailable-capability notes. Admit after runtime
// tool registration and before prompt injection, or refresh an earlier rendering
// with RefreshPreloadedInstructions. Callbacks must not reenter Setup methods.
func (s *Setup) Preload(admit PreloadAdmission) (string, error) {
	if s == nil || s.Registry == nil {
		return "", nil
	}
	s.preloadOnce.Do(func() {
		seen := make(map[string]bool)
		var instructions []string
		for _, name := range s.preloadNames {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			activation, err := NewActivator(s.Registry).Preload(name)
			if err != nil {
				s.preloadErr = errors.Join(s.preloadErr, fmt.Errorf("preload skill %q: %w", name, err))
				continue
			}
			s.preloadActivations = append(s.preloadActivations, activation)
			instructions = append(instructions, promptOnlyPreloadInstructions(activation))
		}
		s.preloadXML = wrapPreloadedInstructions(instructions)
		s.preloadBaseErr = s.preloadErr
		s.preloadRenderings = make(map[string]struct{})
	})
	s.preloadMu.Lock()
	defer s.preloadMu.Unlock()
	if admit != nil {
		s.preloadRenderings[s.preloadXML] = struct{}{}
		s.preloadErr = s.preloadBaseErr
		var instructions []string
		for _, activation := range s.preloadActivations {
			response, err := admit(activation)
			if err != nil {
				s.preloadErr = errors.Join(s.preloadErr, fmt.Errorf("preload skill %q: %w", activation.Skill.Name, err))
				continue
			}
			instructions = append(instructions, response)
		}
		s.preloadXML = wrapPreloadedInstructions(instructions)
	}
	return s.preloadXML, s.preloadErr
}

// PreloadedInstructions returns explicit per-run instructions, bypassing
// auto_invoke, never_auto, catalog budgets, and AGENTS.md suppression. Without a
// prior admitting Preload call this is prompt-only: capabilities are not loaded.
// It never invokes a slash command, expands arguments, or forks a child agent.
func (s *Setup) PreloadedInstructions() (string, error) {
	return s.Preload(nil)
}

// RefreshPreloadedInstructions replaces this Setup's earlier preload block
// after capability admission, including removing rejected skills or refreshing
// another engine's notes. Call before injecting prompt additions again. Prompts
// without a prior block are unchanged. No skill is resolved or admitted again.
func (s *Setup) RefreshPreloadedInstructions(instructions string) string {
	_, _ = s.PreloadedInstructions()
	if s == nil {
		return instructions
	}
	s.preloadMu.RLock()
	defer s.preloadMu.RUnlock()
	for previous := range s.preloadRenderings {
		if previous != "" && previous != s.preloadXML {
			instructions = strings.ReplaceAll(instructions, previous, s.preloadXML)
		}
	}
	return instructions
}

func wrapPreloadedInstructions(instructions []string) string {
	if len(instructions) == 0 {
		return ""
	}
	return "<preloaded_skills>\nThe following skill instructions are already loaded. Do not activate them again.\n\n" + strings.Join(instructions, "\n\n") + "\n</preloaded_skills>"
}

func promptOnlyPreloadInstructions(activation *Activation) string {
	response := RenderActivationInstructions(activation)
	if len(activation.ToolDefs) > 0 {
		response += "\n\nDeclared script tools are not available in this prompt-only context; only the skill instructions are loaded."
	}
	if activation.Skill.CodeMode != nil {
		response += fmt.Sprintf("\n\nThis skill also ships a code mode module (skill:%s), which is not available in this prompt-only context. Follow the instructions above without it.", activation.Skill.Name)
	}
	return response
}
