package serve

import (
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/mcphttp"
)

func serveMCPToolResult(out core.ToolOutput) mcphttp.ToolResult {
	return mcphttp.ToolResult{Content: out.Content, IsError: out.IsError || out.TimedOut}
}
