package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	appchat "github.com/samsaffron/term-llm/internal/app/chat"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

type chatInvocation struct {
	opts             appchat.Options
	deps             appchat.Dependencies
	handoverApproval *tools.ApprovalMode
}

func (r *chatInvocation) ownsTerminalHost() bool     { return r.deps.Interactive() }
func (r *chatInvocation) rendererOwnsTerminal() bool { return r.deps.OutputInteractive() }

func legacyTerminalProgressEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Chat.TerminalProgress {
		return false
	}
	mode, _ := chat.ParseTerminalTitleMode(cfg.Chat.TerminalTitle)
	return mode == chat.TerminalTitleSmart
}

type chatMCPManager interface {
	SetSamplingProvider(provider core.Provider, model string, yoloMode bool)
	Enable(ctx context.Context, name string) error
}

func configureChatMCPServers(ctx context.Context, manager chatMCPManager, provider core.Provider, model string, yoloMode bool, serversCSV string, warnings io.Writer) {
	// Sampling capability is negotiated during MCP initialization, so the handler
	// must exist before the first client is enabled.
	manager.SetSamplingProvider(provider, model, yoloMode)
	for _, server := range strings.Split(serversCSV, ",") {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		if err := manager.Enable(ctx, server); err != nil && warnings != nil {
			fmt.Fprintf(warnings, "Warning: failed to enable MCP server '%s': %v\n", server, err)
		}
	}
}
