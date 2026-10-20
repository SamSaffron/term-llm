package cmd

import (
	"context"
	"io"

	"github.com/samsaffron/term-llm/internal/app/runtime/mcpstart"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/mcp"
)

// MCP startup adapters remain for the CLI-owned edit/loop bodies.

// MCPOptions contains options for enabling MCP servers.
type MCPOptions = mcpstart.MCPOptions

func enableMCPServersWithFeedback(ctx context.Context, mcpFlag string, engine *engine.Engine, errWriter io.Writer, opts *MCPOptions) (*mcp.Manager, error) {
	return mcpstart.EnableMCPServersWithFeedback(ctx, mcpFlag, engine, errWriter, opts)
}
