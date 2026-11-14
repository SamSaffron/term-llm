package memory

import "github.com/term-llm/term-llm/internal/llm/core"

// Mining tools belong to the dedicated collector/budget scope, not script cells.
func (*memorySearchFragmentsTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "mining_scope"}
}

func (*memoryListFragmentsTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "mining_scope"}
}

func (*memoryGetFragmentTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "mining_scope"}
}

func (*memoryCreateFragmentTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "mining_scope"}
}

func (*memoryUpdateFragmentTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "mining_scope"}
}
