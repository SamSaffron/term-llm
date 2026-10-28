package skills

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// GateProjectScriptTool wraps a project skill tool when config has not opted in.
// Checking yolo at execution preserves mode toggles for already-activated tools.
func GateProjectScriptTool(tool core.Tool, yoloEnabled func() bool) core.Tool {
	return &projectScriptTool{Tool: tool, yoloEnabled: yoloEnabled}
}

type projectScriptTool struct {
	core.Tool
	yoloEnabled func() bool
}

func (t *projectScriptTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	if t.yoloEnabled == nil || !t.yoloEnabled() {
		output := core.TextOutput(fmt.Sprintf("project skill tool %q is disabled; enable skills.allow_project_script_tools or --allow-project-skill-tools to run project skill scripts", t.Spec().Name))
		output.IsError = true
		return output, nil
	}
	return t.Tool.Execute(ctx, args)
}
