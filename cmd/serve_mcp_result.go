package cmd

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/mcphttp"
)

func serveMCPToolResult(out core.ToolOutput) mcphttp.ToolResult {
	return mcphttp.ToolResult{Content: out.Content, IsError: out.IsError || out.TimedOut}
}
