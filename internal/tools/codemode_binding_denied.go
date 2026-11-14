package tools

import (
	"context"
	"encoding/json"

	"github.com/term-llm/term-llm/internal/llm/core"
)

// codeModeUnavailableTool denies one affected tool inside a script cell without
// disabling unrelated calls. It is only reachable through a captured authority
// (the live name is shadowed); never use it for a tool a cell could otherwise
// reach through a live fallback for scoped effects.
type codeModeUnavailableTool struct {
	name   string
	reason string
}

func (t *codeModeUnavailableTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: t.name, Description: "Unavailable in this script cell: " + t.reason}
}

func (t *codeModeUnavailableTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	output := core.TextOutput(t.name + " is unavailable in this script cell: " + t.reason)
	output.IsError = true
	return output, nil
}

func (t *codeModeUnavailableTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "binding_unavailable"}
}

func (t *codeModeUnavailableTool) Preview(json.RawMessage) string { return t.name + " (unavailable)" }
