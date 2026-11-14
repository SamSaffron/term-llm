package tool

import "github.com/term-llm/term-llm/internal/llm/core"

// Script lifecycle tools are never nested capabilities.
func (*Tool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "script_lifecycle"}
}

func (*ExecTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "script_lifecycle"}
}

func (*WaitTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "script_lifecycle"}
}

func (*CancelTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "script_lifecycle"}
}
func (*Tool) IsCodeModeLifecycleTool() bool       { return true }
func (*ExecTool) IsCodeModeLifecycleTool() bool   { return true }
func (*WaitTool) IsCodeModeLifecycleTool() bool   { return true }
func (*CancelTool) IsCodeModeLifecycleTool() bool { return true }

func (*Tool) ToolOrigin() core.ToolOrigin {
	return core.ToolOrigin{Class: core.ToolOriginBuiltin, ID: Name}
}

func (*ExecTool) ToolOrigin() core.ToolOrigin {
	return core.ToolOrigin{Class: core.ToolOriginBuiltin, ID: ExecName}
}

func (*WaitTool) ToolOrigin() core.ToolOrigin {
	return core.ToolOrigin{Class: core.ToolOriginBuiltin, ID: WaitName}
}

func (*CancelTool) ToolOrigin() core.ToolOrigin {
	return core.ToolOrigin{Class: core.ToolOriginBuiltin, ID: CancelName}
}
