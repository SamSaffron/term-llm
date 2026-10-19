package cmd

import (
	"context"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
)

// Run starts serve from captured invocation values without exposing host test seams.
func Run(ctx context.Context, opts *appserve.Options, platforms []string, invocation appserve.Invocation) error {
	return runServeWith(ctx, serveDeps{Invocation: invocation}, opts, platforms)
}

// RunMCP starts the MCP surface with its deferred approval resolver.
func RunMCP(ctx context.Context, opts *appserve.MCPOptions, args []string, invocation appserve.MCPInvocation) error {
	return runServeMCPWithOptions(ctx, invocation, args, opts)
}

// RunHub starts the Hub host; its leaf invocation contains only plain values.
func RunHub(ctx context.Context, opts apphub.Options, invocation apphub.Invocation) error {
	return runServeHubWith(ctx, invocation, &opts)
}
