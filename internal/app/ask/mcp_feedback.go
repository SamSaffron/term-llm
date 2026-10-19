package ask

import (
	"context"
	"io"
	"time"

	"github.com/samsaffron/term-llm/internal/app/runtime/mcpstart"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/mcp"
)

// MCPOptions contains options for enabling MCP servers.
type MCPOptions = mcpstart.MCPOptions

// EnableMCPServersWithFeedback initializes MCP servers with user feedback.
func EnableMCPServersWithFeedback(ctx context.Context, mcpFlag string, engine *engine.Engine, errWriter io.Writer, opts *MCPOptions) (*mcp.Manager, error) {
	return mcpstart.EnableMCPServersWithFeedback(ctx, mcpFlag, engine, errWriter, opts)
}

func CheckMCPServerFailures(manager *mcp.Manager, serverNames []string, feedback io.Writer, animate bool) error {
	return mcpstart.CheckMCPServerFailures(manager, serverNames, feedback, animate)
}

func WaitForMCPStartup(ctx context.Context, manager interface {
	ServerStatus(string) (mcp.ServerStatus, error)
}, serverNames []string, feedback io.Writer, animate bool, maxWait time.Duration) error {
	return mcpstart.WaitForMCPStartup(ctx, manager, serverNames, feedback, animate, maxWait)
}

// ParseServerList splits comma-separated server names and trims whitespace.
func ParseServerList(mcpFlag string) []string { return mcpstart.ParseServerList(mcpFlag) }
