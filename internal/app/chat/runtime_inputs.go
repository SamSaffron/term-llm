package chat

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/term-llm/term-llm/internal/authoridentity"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/tools"
	"github.com/term-llm/term-llm/internal/tui/chat"
)

type chatInvocation struct {
	user             *authoridentity.Selection
	opts             Options
	deps             Dependencies
	handoverApproval *tools.ApprovalMode
}

func (r *chatInvocation) ownsTerminalHost() bool     { return r.deps.Interactive() }
func (r *chatInvocation) rendererOwnsTerminal() bool { return r.deps.OutputInteractive() }

// LegacyTerminalProgressEnabled retains the deprecated smart-title-only progress scope.
func LegacyTerminalProgressEnabled(cfg *config.Config) bool {
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
