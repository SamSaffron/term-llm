package engine

import "github.com/samsaffron/term-llm/internal/llm/core"

// The fake used specifically by script execution tests explicitly opts in.
// General engine fixtures and undeclared production tools remain default-denied.
func (codeTestTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Allowed: true}
}

// Policy/description adapters in these tests represent replacements by the same
// trusted fixture service, not unrelated tool registrations stealing its name.
func (codeTestTool) ToolOrigin() core.ToolOrigin {
	return core.ToolOrigin{Class: core.ToolOriginHost, ID: "codemode-test"}
}

type policyCodeTestTool struct {
	codeTestTool
	exposure core.CodeModeExposure
}

func (t policyCodeTestTool) CodeModeExposure() core.CodeModeExposure { return t.exposure }
