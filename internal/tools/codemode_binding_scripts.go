package tools

import (
	"maps"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/skills"
)

// bindCodeModeScriptTool rebuilds a registered custom agent or skill script
// against the cell's captured config, limits and source directory. Project skill
// scripts keep their trust gate, but the gate now consults the cell's captured
// approval snapshot rather than future yolo changes on the live registry.
func bindCodeModeScriptTool(original core.Tool, shadow *LocalToolRegistry) core.Tool {
	if original == nil {
		return &codeModeUnavailableTool{name: "unknown_script", reason: codeModeBindingUnavailableCode + ": empty script registration"}
	}
	name := original.Spec().Name
	switch script := original.(type) {
	case *CustomScriptTool:
		return cloneCodeModeScriptTool(script, shadow)
	default:
		inner, _, gated := skills.ProjectScriptToolBinding(original)
		if !gated {
			return &codeModeUnavailableTool{name: name, reason: codeModeBindingUnavailableCode + ": unsupported local script adapter"}
		}
		gatedScript, ok := inner.(*CustomScriptTool)
		if !ok || gatedScript == nil {
			return &codeModeUnavailableTool{name: name, reason: codeModeBindingUnavailableCode + ": unsupported gated script adapter"}
		}
		bound := cloneCodeModeScriptTool(gatedScript, shadow)
		return skills.GateProjectScriptTool(bound, shadow.codeModeYoloEnabled)
	}
}

func cloneCodeModeScriptTool(original *CustomScriptTool, shadow *LocalToolRegistry) *CustomScriptTool {
	clone := newCustomScriptTool(cloneCodeModeToolDef(original.def), original.agentDir, shadow.limits, shadow.config)
	if origin := original.ToolOrigin(); origin.Class != "" {
		clone.SetToolOrigin(origin)
	}
	return clone
}

func cloneCodeModeToolDef(def agents.CustomToolDef) agents.CustomToolDef {
	def.Env = maps.Clone(def.Env)
	def.Input = maps.Clone(def.Input)
	return def
}

// codeModeYoloEnabled reports the captured approval snapshot. Project skill
// trust cannot widen after admission, and later live root narrowing still
// applies through the captured approval manager.
func (r *LocalToolRegistry) codeModeYoloEnabled() bool {
	return r != nil && r.approval != nil && r.approval.YoloEnabled()
}
