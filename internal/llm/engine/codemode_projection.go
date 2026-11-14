package engine

import "github.com/term-llm/term-llm/internal/llm/core"

func projectCellOutput(output core.ToolOutput, maxChars int) core.ToolOutput {
	return core.ProjectCodeModeOutput(output, maxChars)
}
